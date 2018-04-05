package local

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yinqiwen/gsnova/common/channel"
	"github.com/yinqiwen/gsnova/common/dns"
	"github.com/yinqiwen/gsnova/common/helper"
	"github.com/yinqiwen/gsnova/common/hosts"
	"github.com/yinqiwen/gsnova/common/logger"
	"github.com/yinqiwen/gsnova/common/mux"
	"github.com/yinqiwen/gsnova/common/socks"
)

var proxyServerRunning = true

var runningProxyStreamCount int64
var runningProxyConns sync.Map

func serveProxyConn(conn net.Conn, remoteHost, remotePort string, proxy *ProxyConfig) {
	var proxyChannelName string
	protocol := "tcp"
	localConn := conn
	atomic.AddInt64(&runningProxyStreamCount, 1)
	runningProxyConns.Store(conn, true)
	defer localConn.Close()
	defer atomic.AddInt64(&runningProxyStreamCount, -1)
	defer runningProxyConns.Delete(conn)

	isSocksProxy := false
	isHttpsProxy := false
	isHttp11Proto := false
	isTransparentProxy := len(remoteHost) > 0
	var initialHTTPReq *http.Request

	var bufconn *bufio.Reader
	if !isTransparentProxy {
		socksConn, sbufconn, err := socks.NewSocksConn(conn)
		if nil == err {
			isSocksProxy = true
			logger.Debug("Local proxy recv %s proxy conn to %s", socksConn.Version(), socksConn.Req.Target)
			socksConn.Grant(&net.TCPAddr{
				IP: net.ParseIP("0.0.0.0"), Port: 0})
			localConn = socksConn
			if socksConn.Req.Target == GConf.UDPGW.Addr {
				logger.Debug("Handle udpgw conn for %v", socksConn.Req.Target)
				handleUDPGatewayConn(localConn, proxy)
				return
			}

			remoteHost, remotePort, err = net.SplitHostPort(socksConn.Req.Target)
			if nil != err {
				logger.Error("Invalid socks target addresss:%s with reason %v", socksConn.Req.Target, err)
				return
			}
			bufconn = sbufconn
		} else {
			if nil == sbufconn {
				localConn.Close()
				return
			}
			bufconn = sbufconn
		}
	}

	if nil == bufconn {
		bufconn = bufio.NewReader(localConn)
	}

	trySniffDomain := false
	if len(remoteHost) == 0 || (net.ParseIP(remoteHost) != nil && !helper.IsPrivateIP(remoteHost)) {
		//this is a ip from local dns query
		trySniffDomain = true
	}

	//1. sniff SNI first
	if (isSocksProxy || isTransparentProxy) && trySniffDomain {
		if remotePort == "80" {
			conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
		} else {
			conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		}

		if nil != dns.CNIPSet {
			remoteIP := net.ParseIP(remoteHost)
			logger.Debug("Recv proxy request to IP:%v CNIP:%v", remoteIP, dns.CNIPSet.IsInCountry(remoteIP, "CN"))
		}
		sni, err := helper.PeekTLSServerName(bufconn)
		if nil != err {
			//logger.Debug("##Failed to sniff SNI with error:%v", err)
		} else {
			if redirect, ok := GConf.SNI.redirect(sni); ok {
				sni = redirect
			}
			logger.Debug("Sniffed SNI:%s:%s for IP:%s:%s", sni, remotePort, remoteHost, remotePort)
			remoteHost = sni
			trySniffDomain = false
		}
	}

	//2. sniff domain via http
	if trySniffDomain {
		localConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		headChunk, err := bufconn.Peek(7)
		if len(headChunk) != 7 {
			if err != io.EOF {
				logger.Error("Peek:%s %d %v to %s:%s", string(headChunk), len(headChunk), err, remoteHost, remotePort)
			}
			goto START
		}
		method := string(headChunk)
		if tmp := strings.Fields(method); len(tmp) > 0 {
			method = tmp[0]
		}
		method = strings.ToUpper(method)
		switch method {
		case "GET":
			fallthrough
		case "POST":
			fallthrough
		case "HEAD":
			fallthrough
		case "PUT":
			fallthrough
		case "DELETE":
			fallthrough
		case "CONNECT":
			fallthrough
		case "OPTIONS":
			fallthrough
		case "TRACE":
			fallthrough
		case "PATCH":
			isHttp11Proto = true
		default:
			isHttp11Proto = false
		}
		//log.Printf("[%d]]Method:%s", sid, method)
		if isHttp11Proto {
			initialHTTPReq, err = http.ReadRequest(bufconn)
			if nil != err {
				logger.Error("Read first request failed from proxy connection for reason:%v", err)
				return
			}
			//log.Printf("Host:%s %v", initialHTTPReq.Host, initialHTTPReq.URL)
			if strings.Contains(initialHTTPReq.Host, ":") {
				remoteHost, remotePort, _ = net.SplitHostPort(initialHTTPReq.Host)
			} else {
				remoteHost = initialHTTPReq.Host
				remotePort = "80"
				if strings.EqualFold(initialHTTPReq.Method, "CONNECT") {
					remotePort = "443"
				}
			}
			if strings.EqualFold(initialHTTPReq.Method, "CONNECT") {
				protocol = "https"
				if !isSocksProxy {
					localConn.Write([]byte("HTTP/1.0 200 Connection established\r\n\r\n"))
					isHttpsProxy = true
					initialHTTPReq = nil
				}
			} else {
				protocol = "http"
			}
		} else {
			if !isHttp11Proto && !isSocksProxy && !isTransparentProxy {
				logger.Error("[ERROR]Can NOT handle non HTTP1.1 proto in non socks proxy mode %v:%v:%v  %v to %v", isHttp11Proto, isSocksProxy, isTransparentProxy, conn.LocalAddr(), conn.RemoteAddr())
				return
			}
		}
	}
START:
	if len(remoteHost) == 0 || len(remotePort) == 0 {
		logger.Error("Can NOT resolve remote host or port %s:%s %v", remoteHost, remotePort, initialHTTPReq)
		return
	}
	proxyChannelName = proxy.getProxyChannelByHost(protocol, remoteHost)

	if len(proxyChannelName) == 0 {
		logger.Error("[ERROR]No proxy found for %s:%s", protocol, remoteHost)
		return
	}
	stream, conf, err := channel.GetMuxStreamByChannel(proxyChannelName)
	if nil != err || nil == stream {
		logger.Error("Failed to open stream for reason:%v by proxy:%s", err, proxyChannelName)
		return
	}
	defer stream.Close()
	ssid := stream.StreamID()
	opt := mux.StreamOptions{
		DialTimeout: conf.RemoteDialMSTimeout,
		Hops:        conf.Hops,
	}

	if remotePort == "443" && nil == net.ParseIP(remoteHost) {
		remoteSNI := conf.GetRemoteSNI(remoteHost)
		if len(remoteSNI) > 0 {
			sniHost := hosts.GetHost(remoteSNI)
			logger.Notice("Proxy stream[%d] select remote SNI host %s for proxy to %s:%s", ssid, sniHost, remoteHost, remotePort)
			remoteHost = sniHost
		}
	}

	logger.Notice("Proxy stream[%d] select %s for proxy to %s:%s", ssid, proxyChannelName, remoteHost, remotePort)
	err = stream.Connect("tcp", net.JoinHostPort(remoteHost, remotePort), opt)
	if nil != err {
		logger.Error("Connect failed from proxy connection for reason:%v", err)
		return
	}

	//clear read timeout
	var zero time.Time
	localConn.SetReadDeadline(zero)
	streamReader, streamWriter := mux.GetCompressStreamReaderWriter(stream, conf.Compressor)

	closeCh := make(chan int, 1)
	go func() {
		buf := make([]byte, 128*1024)
		io.CopyBuffer(localConn, streamReader, buf)
		closeCh <- 1
	}()

	//start task to check stream timeout(if the stream has no read&write action more than 10s)
	timeoutTicker := time.NewTicker(2 * time.Second)
	stopTicker := make(chan bool, 1)
	go func() {
		maxIdleTime := time.Duration(GConf.Mux.StreamIdleTimeout) * time.Second
		if maxIdleTime == 0 {
			maxIdleTime = 10 * time.Second
		}

		for {
			select {
			case <-timeoutTicker.C:
				if time.Now().Sub(stream.LatestIOTime()) > maxIdleTime {
					localConn.Close()
					stream.Close()
					logger.Error("Close stream[%d] since it's not active since %v ago.", ssid, time.Now().Sub(stream.LatestIOTime()))
					return
				}
			case <-stopTicker:
				return
			}
		}
	}()
	if (isSocksProxy || isHttpsProxy || isTransparentProxy) && nil == initialHTTPReq {
		buf := make([]byte, 128*1024)
		io.CopyBuffer(streamWriter, bufconn, buf)
		if close, ok := streamWriter.(io.Closer); ok {
			close.Close()
		}
	} else {
		proxyReq := initialHTTPReq
		initialHTTPReq = nil
		for {
			if nil != proxyReq {
				proxyReq.Header.Del("Proxy-Connection")
				proxyReq.Header.Del("Proxy-Authorization")
				err = proxyReq.Write(streamWriter)
				if nil != err {
					logger.Error("Failed to write http request for reason:%v", err)
					return
				}
			}
			prevReq := proxyReq
			//localConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			proxyReq, err = http.ReadRequest(bufconn)
			if nil != err {
				if err, ok := err.(net.Error); ok && err.Timeout() {

				}
				if err != io.EOF && !strings.Contains(err.Error(), "use of closed network connection") {
					logger.Notice("Failed to read proxy http request to %s:%s for reason:%v", remoteHost, remotePort, err)
				}
				return
			}
			if nil != prevReq && prevReq.Host != proxyReq.Host {
				logger.Debug("Switch to next stream since target host change from %s to %s", prevReq.Host, proxyReq.Host)
				stream.Close()
				goto START
			}
		}
	}
	<-closeCh
	close(stopTicker)
}

func startLocalProxyServer(proxyIdx int) (*net.TCPListener, error) {
	proxyConf := &GConf.Proxy[proxyIdx]
	if supportTransparentProxy() {
		go startTransparentUDProxy(proxyConf.Local, proxyConf)
	}
	tcpaddr, err := net.ResolveTCPAddr("tcp", proxyConf.Local)
	if nil != err {
		logger.Fatal("[ERROR]Local server address:%s error:%v", proxyConf.Local, err)
		return nil, err
	}
	var lp *net.TCPListener
	lp, err = net.ListenTCP("tcp", tcpaddr)
	if nil != err {
		logger.Fatal("Can NOT listen on address:%s", proxyConf.Local)
		return nil, err
	}
	logger.Info("Listen on address %s", proxyConf.Local)
	runningServers[proxyIdx] = lp
	go func() {
		for proxyServerRunning {
			var conn net.Conn
			conn, err = lp.AcceptTCP()
			if nil != err {
				continue
			}
			isTransparent := false
			if supportTransparentProxy() {
				tcpAddr, ok := conn.RemoteAddr().(*net.TCPAddr)
				if ok {
					_, exist := helper.GetLocalIPSet()[tcpAddr.IP.String()]
					if !exist {
						isTransparent = true
					}
				}
			}
			var originalHost, originalPort string
			if isTransparent {
				newConn, remoteIP, remotePort, err := getOrinalTCPRemoteAddr(conn)
				if nil != err {
					logger.Error("Failed to get original address for transparent proxy:%v", err)
					continue
				}
				conn = newConn
				originalHost = remoteIP.String()
				originalPort = fmt.Sprintf("%d", remotePort)
			}
			go serveProxyConn(conn, originalHost, originalPort, &GConf.Proxy[proxyIdx])
		}
		lp.Close()
	}()
	return lp, nil
}

var runningServers []*net.TCPListener

func startLocalServers() error {
	proxyServerRunning = true
	runningServers = make([]*net.TCPListener, len(GConf.Proxy))
	for i := range GConf.Proxy {
		startLocalProxyServer(i)
	}
	return nil
}

func stopLocalServers() {
	proxyServerRunning = false
	for _, l := range runningServers {
		if nil != l {
			l.Close()
		}
	}
	//closeAllProxySession()
	closeAllUDPSession()
	runningProxyConns.Range(func(key, value interface{}) bool {
		conn := key.(net.Conn)
		if nil != conn {
			conn.Close()
		}
		return true
	})
}
