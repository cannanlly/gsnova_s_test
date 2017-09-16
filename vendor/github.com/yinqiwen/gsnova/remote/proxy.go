package remote

import (
	"io"
	"net"
	"strings"
	"time"

	"github.com/yinqiwen/gsnova/common/logger"
	"github.com/yinqiwen/gsnova/common/mux"
)

func handleProxyStream(stream mux.MuxStream, compresor string) {
	creq, err := mux.ReadConnectRequest(stream)
	if nil != err {
		stream.Close()
		logger.Error("[ERROR]:Failed to read connect request:%v", err)
		return
	}
	logger.Debug("[%d]Start handle stream:%v with comprresor:%s", stream.StreamID(), creq, compresor)
	timeout := ServerConf.DialTimeout
	if timeout == 0 {
		timeout = 10
	}
	c, err := net.DialTimeout(creq.Network, creq.Addr, time.Duration(timeout)*time.Second)
	if nil != err {
		logger.Error("[ERROR]:Failed to connect %s:%v for reason:%v", creq.Network, creq.Addr, err)
		stream.Close()
		return
	}
	streamReader, streamWriter := mux.GetCompressStreamReaderWriter(stream, compresor)
	if strings.EqualFold(creq.Network, "udp") {
		//udp connection need to set read timeout to avoid hang forever
		udpReadTimeout := 30 * time.Second
		if ServerConf.UDPReadTimeout > 0 {
			udpReadTimeout = time.Duration(ServerConf.UDPReadTimeout) * time.Second
		}
		c.SetReadDeadline(time.Now().Add(udpReadTimeout))
	}
	defer c.Close()
	go func() {
		io.Copy(c, streamReader)
	}()
	io.Copy(streamWriter, c)

	if close, ok := streamWriter.(io.Closer); ok {
		close.Close()
	}
	if close, ok := streamReader.(io.Closer); ok {
		close.Close()
	}
	//n, err := io.Copy(stream, c)

}

func ServProxyMuxSession(session mux.MuxSession) error {
	isAuthed := false
	compressor := mux.SnappyCompressor
	for {
		stream, err := session.AcceptStream()
		if nil != err {
			//session.Close()
			logger.Error("Failed to accept stream with error:%v", err)
			return err
		}
		if !isAuthed {
			auth, err := mux.ReadAuthRequest(stream)
			if nil != err {
				logger.Error("[ERROR]:Failed to read auth request:%v", err)
				continue
			}
			logger.Info("Recv auth:%v", auth)
			if !ServerConf.VerifyUser(auth.User) {
				session.Close()
				return mux.ErrAuthFailed
			}
			if !mux.IsValidCompressor(auth.CompressMethod) {
				logger.Error("[ERROR]Invalid compressor:%s", auth.CompressMethod)
				session.Close()
				return mux.ErrAuthFailed
			}
			compressor = auth.CompressMethod
			isAuthed = true
			authRes := &mux.AuthResponse{Code: mux.AuthOK}
			mux.WriteMessage(stream, authRes)
			stream.Close()
			if tmp, ok := session.(*mux.ProxyMuxSession); ok {
				tmp.Session.ResetCryptoContext(auth.CipherMethod, auth.CipherCounter)
			}
			continue
		}
		go handleProxyStream(stream, compressor)
	}
}
