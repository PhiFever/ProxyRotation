package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// RFC 1928（SOCKS5）与 RFC 1929（用户名/密码子协商）的常量。
//
// 只实现 CONNECT：它与 dialUpstream 返回的 TCP 隧道一一对应，接上去就是 pipe。
// BIND 要求上游允许反向连接，短效商业代理没有一家给；UDP ASSOCIATE 要自己开 UDP 端口
// 再找上游转发，上游是 http 时无路可走、是 socks5 时代理商也基本不开 UDP——做了也是假的。
const (
	socks5Version = 0x05
	authVersion   = 0x01 // RFC 1929 子协商自带版本号，与 socks5Version 无关

	methodNoAuth       = 0x00
	methodUserPass     = 0x02
	methodNoAcceptable = 0xFF

	authSuccess = 0x00
	authFailure = 0x01

	cmdConnect = 0x01

	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04

	// 失败一律回 0x01，与"失败的请求一律对客户端返回 502"是同一个口径：我们手上的错误
	// 是"经上游代理拨号失败"，本来就分不清目标不可达还是上游死了，细分 0x03/0x04/0x05
	// 只能靠猜。
	repSuccess        = 0x00
	repGeneralFail    = 0x01
	repCmdNotSupport  = 0x07
	repAtypNotSupport = 0x08
)

// HandleSOCKS5 处理一条已被 sniffListener 判为 SOCKS5 的入站连接（首字节尚未消费）。
// 握手之后的每一步都与 handleConnect 对称：同一个 Rotator、同一套统计、同一条自愈路径。
func (s *ProxyServer) HandleSOCKS5(conn net.Conn) {
	target, ok := s.socks5Handshake(conn)
	if !ok {
		conn.Close()
		return
	}

	upstream, err := s.rotator.Get()
	if err != nil {
		logUpstreamFail("SOCKS5", target, upstream, err)
		socks5Reply(conn, repGeneralFail)
		conn.Close()
		return
	}

	upstreamConn, err := dialUpstream(s.cfg.Via, upstream, target)
	if err != nil {
		logUpstreamFail("SOCKS5", target, upstream, err)
		s.rotator.MarkFailed()
		socks5Reply(conn, repGeneralFail)
		conn.Close()
		return
	}

	if err := socks5Reply(conn, repSuccess); err != nil {
		conn.Close()
		upstreamConn.Close()
		return
	}

	s.rotator.OnSuccess()
	s.stats.OnRequest()
	s.pipe(conn, upstreamConn)
}

// socks5Handshake 走完方法协商、可选的账号校验和 CONNECT 请求，返回目标 host:port。
func (s *ProxyServer) socks5Handshake(conn net.Conn) (string, bool) {
	conn.SetDeadline(time.Now().Add(clientHandshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	// 方法协商：VER NMETHODS METHODS...
	head := make([]byte, 2)
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != socks5Version {
		return "", false
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return "", false
	}
	if !s.socks5Negotiate(conn, methods) {
		return "", false
	}

	// 请求：VER CMD RSV ATYP DST.ADDR DST.PORT
	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return "", false
	}
	if req[1] != cmdConnect {
		socks5Reply(conn, repCmdNotSupport)
		return "", false
	}
	host, err := readSOCKS5Host(conn, req[3])
	if err != nil {
		socks5Reply(conn, repAtypNotSupport)
		return "", false
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return "", false
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))), true
}

// socks5Negotiate 选定认证方法，两个方向都只认一种，不做降级。
//
// 配了 auth 时只接受 0x02：SOCKS5 的方法协商是双向承诺，回 0x00 等于给一个自认为设了
// 密码的端口开免密后门——HTTP 侧的门在 checkInboundAuth，走的是完全不同的代码路径，
// 忘了接这里不会有任何报错，而 listen 默认是全网卡。
//
// 没配 auth 时只接受 0x00：回 0x02 再无条件放行是对协议撒谎——服务端宣告"我要验密码"
// 却根本没验。宁可让客户端收到明确的 0xFF。这与 HTTP 侧"没配 auth 就不看
// Proxy-Authorization"的宽松行为不同，但那不是不一致：Proxy-Authorization 是单向的
// 请求头，忽略它不构成任何承诺。
func (s *ProxyServer) socks5Negotiate(conn net.Conn, methods []byte) bool {
	want := byte(methodNoAuth)
	if s.cfg.Auth.Username != "" {
		want = methodUserPass
	}
	if bytes.IndexByte(methods, want) < 0 {
		conn.Write([]byte{socks5Version, methodNoAcceptable})
		return false
	}
	if _, err := conn.Write([]byte{socks5Version, want}); err != nil {
		return false
	}
	if want == methodNoAuth {
		return true
	}
	return s.socks5Auth(conn)
}

// socks5Auth 走 RFC 1929：VER ULEN UNAME PLEN PASSWD。
func (s *ProxyServer) socks5Auth(conn net.Conn) bool {
	head := make([]byte, 2) // VER ULEN
	if _, err := io.ReadFull(conn, head); err != nil || head[0] != authVersion {
		return false
	}
	user := make([]byte, head[1])
	if _, err := io.ReadFull(conn, user); err != nil {
		return false
	}
	plen := make([]byte, 1)
	if _, err := io.ReadFull(conn, plen); err != nil {
		return false
	}
	pass := make([]byte, plen[0])
	if _, err := io.ReadFull(conn, pass); err != nil {
		return false
	}

	if string(user) != s.cfg.Auth.Username || string(pass) != s.cfg.Auth.Password {
		conn.Write([]byte{authVersion, authFailure})
		return false
	}
	_, err := conn.Write([]byte{authVersion, authSuccess})
	return err == nil
}

// readSOCKS5Host 读 DST.ADDR。
//
// 域名原样返回、交给上游代理去解析，与 CONNECT 路径直接传 r.Host 是同一个口径——DNS
// 发生在出口侧。客户端发来 IPv4/IPv6 说明它已经在本地解析过了（配的是 socks5:// 而非
// socks5h://），那次泄露发生在我们之前，无从补救，只能在文档里提醒。
func readSOCKS5Host(r io.Reader, atyp byte) (string, error) {
	switch atyp {
	case atypIPv4, atypIPv6:
		n := net.IPv4len
		if atyp == atypIPv6 {
			n = net.IPv6len
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case atypDomain:
		l := make([]byte, 1)
		if _, err := io.ReadFull(r, l); err != nil {
			return "", err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return string(b), nil
	}
	return "", fmt.Errorf("unsupported socks5 address type %#x", atyp)
}

// socks5Reply 回一条 REP。BND.ADDR / BND.PORT 填全 0：CONNECT 场景下客户端不看它们。
func socks5Reply(conn net.Conn, rep byte) error {
	_, err := conn.Write([]byte{socks5Version, rep, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}
