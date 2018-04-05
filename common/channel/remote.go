package channel

import (
	"io"
	"net"
	"net/url"
	"time"

	"github.com/yinqiwen/pmux"

	"github.com/yinqiwen/gsnova/common/logger"
	"github.com/yinqiwen/gsnova/common/mux"
)

type sessionContext struct {
	activeIOTime time.Time
}

func handleProxyStream(stream mux.MuxStream, auth *mux.AuthRequest, ctx *sessionContext) {
	creq, err := mux.ReadConnectRequest(stream)
	if nil != err {
		stream.Close()
		logger.Error("[ERROR]:Failed to read connect request:%v", err)
		return
	}
	logger.Debug("[%d]Start handle stream:%v with comprresor:%s", stream.StreamID(), creq, auth.CompressMethod)

	var c io.ReadWriteCloser
	dialTimeout := creq.DialTimeout
	if dialTimeout == 0 {
		dialTimeout = 10000
	}
	if len(creq.Hops) == 0 {
		var conn net.Conn
		conn, err = net.DialTimeout(creq.Network, creq.Addr, time.Duration(dialTimeout)*time.Millisecond)
		if nil != err {
			logger.Error("[ERROR]:Failed to connect %s:%v for reason:%v", creq.Network, creq.Addr, err)
		} else {
			if creq.ReadTimeout > 0 {
				//connection need to set read timeout to avoid hang forever
				readTimeout := time.Duration(creq.ReadTimeout) * time.Millisecond
				conn.SetReadDeadline(time.Now().Add(readTimeout))
			}
			c = conn
		}
	} else {
		var nextURL *url.URL
		var nextStream mux.MuxStream
		next := creq.Hops[0]
		nextHops := creq.Hops[1:]
		nextURL, err = url.Parse(next)
		if nil == err {
			nextStream, _, err = GetMuxStreamByURL(nextURL, auth.User, &DefaultServerCipher)
			if nil == err {
				opt := mux.StreamOptions{
					DialTimeout: creq.DialTimeout,
					ReadTimeout: creq.ReadTimeout,
					Hops:        nextHops,
				}
				err = nextStream.Connect(creq.Network, creq.Addr, opt)
				if nil == err {
					c = nextStream
				} else {
					logger.Error("[ERROR]:Failed to connect next:%s for reason:%v", next, err)
				}
			}
		} else {
			logger.Error("Failed to parse proxy url:%s with reason:%v", next, err)
		}
	}

	if nil != err {
		stream.Close()
		return
	}
	streamReader, streamWriter := mux.GetCompressStreamReaderWriter(stream, auth.CompressMethod)
	defer c.Close()
	closeSig := make(chan bool, 2)
	go func() {
		buf := make([]byte, 128*1024)
		io.CopyBuffer(c, streamReader, buf)
		closeSig <- true
	}()
	go func() {
		buf := make([]byte, 128*1024)
		io.CopyBuffer(streamWriter, c, buf)
		closeSig <- true
	}()
	timeoutTicker := time.NewTicker(2 * time.Second)
	stopTicker := make(chan bool, 1)
	go func() {
		for {
			maxIdleTime := time.Duration(defaultMuxConfig.StreamIdleTimeout) * time.Second
			if maxIdleTime == 0 {
				maxIdleTime = 10 * time.Second
			}
			select {
			case <-timeoutTicker.C:
				if stream.LatestIOTime().After(ctx.activeIOTime) {
					ctx.activeIOTime = stream.LatestIOTime()
				}
				if time.Now().Sub(stream.LatestIOTime()) > maxIdleTime {
					c.Close()
					stream.Close()
					return
				}
			case <-stopTicker:
				return
			}
		}
	}()

	<-closeSig
	close(stopTicker)

	if close, ok := streamWriter.(io.Closer); ok {
		close.Close()
	}
	if close, ok := streamReader.(io.Closer); ok {
		close.Close()
	}
}

var DefaultServerCipher CipherConfig

func ServProxyMuxSession(session mux.MuxSession) error {
	var authReq *mux.AuthRequest
	ctx := &sessionContext{}
	ctx.activeIOTime = time.Now()
	defer session.Close()

	if defaultMuxConfig.SessionIdleTimeout > 0 {
		sessionActiveTicker := time.NewTicker(10 * time.Second)
		defer sessionActiveTicker.Stop()

		go func() {
			for range sessionActiveTicker.C {
				ago := time.Now().Sub(ctx.activeIOTime)
				if ago > time.Duration(defaultMuxConfig.SessionIdleTimeout)*time.Second {
					session.Close()
					logger.Error("Close mux session since it's not active since %v ago.", ago)
					return
				}
			}
		}()
	}

	for {
		stream, err := session.AcceptStream()
		if nil != err {
			//session.Close()
			if err != pmux.ErrSessionShutdown {
				logger.Error("Failed to accept stream with error:%v", err)
			}
			return err
		}
		if nil == authReq {
			auth, err := mux.ReadAuthRequest(stream)
			if nil != err {
				logger.Error("[ERROR]:Failed to read auth request:%v", err)
				continue
			}
			logger.Info("Recv auth:%v", auth)
			if !DefaultServerCipher.VerifyUser(auth.User) {
				session.Close()
				return mux.ErrAuthFailed
			}
			if !mux.IsValidCompressor(auth.CompressMethod) {
				logger.Error("[ERROR]Invalid compressor:%s", auth.CompressMethod)
				session.Close()
				return mux.ErrAuthFailed
			}
			authReq = auth
			authRes := &mux.AuthResponse{Code: mux.AuthOK}
			mux.WriteMessage(stream, authRes)
			stream.Close()
			if tmp, ok := session.(*mux.ProxyMuxSession); ok {
				tmp.Session.ResetCryptoContext(auth.CipherMethod, auth.CipherCounter)
			}
			continue
		}
		go handleProxyStream(stream, authReq, ctx)
	}
}
