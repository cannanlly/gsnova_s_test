package quic

import (
	quic "github.com/lucas-clemente/quic-go"
	"github.com/yinqiwen/gsnova/common/helper"
	"github.com/yinqiwen/gsnova/common/logger"
	"github.com/yinqiwen/gsnova/common/mux"
	"github.com/yinqiwen/gsnova/remote"
)

func servQUIC(lp quic.Listener) {
	for {
		sess, err := lp.Accept()
		if nil != err {
			continue
		}
		muxSession := &mux.QUICMuxSession{Session: sess}
		go remote.ServProxyMuxSession(muxSession)
	}
	//ws.WriteMessage(websocket.CloseMessage, []byte{})
}

func StartQuicProxyServer(addr string) error {
	lp, err := quic.ListenAddr(addr, helper.GenerateTLSConfig(), nil)
	if nil != err {
		logger.Error("[ERROR]Failed to listen QUIC address:%s with reason:%v", addr, err)
		return err
	}
	logger.Info("Listen on QUIC address:%s", addr)
	servQUIC(lp)
	return nil
}
