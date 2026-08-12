package main

import (
	"encoding/base64"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// hopByHopHeaders 不应转发给上游/客户端。
var hopByHopHeaders = []string{
	"Proxy-Authorization",
	"Proxy-Connection",
	"Connection",
	"Keep-Alive",
	"Transfer-Encoding",
	"Te",
	"Trailer",
	"Upgrade",
}

type ProxyServer struct {
	cfg     *Config
	rotator *Rotator
	stats   *Stats
}

func NewProxyServer(cfg *Config, r *Rotator, s *Stats) *ProxyServer {
	return &ProxyServer{cfg: cfg, rotator: r, stats: s}
}

func (s *ProxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.checkInboundAuth(r) {
		w.Header().Set("Proxy-Authenticate", `Basic realm="proxyrotation"`)
		http.Error(w, "", http.StatusProxyAuthRequired)
		return
	}
	if r.Method == http.MethodConnect {
		s.handleConnect(w, r)
		return
	}
	s.handleHTTP(w, r)
}

func (s *ProxyServer) checkInboundAuth(r *http.Request) bool {
	if s.cfg.Auth.Username == "" {
		return true
	}
	const prefix = "Basic "
	header := r.Header.Get("Proxy-Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(header[len(prefix):])
	if err != nil {
		return false
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	return ok && user == s.cfg.Auth.Username && pass == s.cfg.Auth.Password
}

// handleConnect 处理 CONNECT 隧道，覆盖 HTTPS——这是主路径。
func (s *ProxyServer) handleConnect(w http.ResponseWriter, r *http.Request) {
	upstream, err := s.rotator.Get()
	if err != nil {
		logUpstreamFail(r, upstream, err)
		http.Error(w, "no upstream proxy available", http.StatusBadGateway)
		return
	}

	upstreamConn, err := dialUpstream(s.cfg.Via, upstream, r.Host)
	if err != nil {
		logUpstreamFail(r, upstream, err)
		s.rotator.MarkFailed()
		http.Error(w, "upstream dial failed", http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		upstreamConn.Close()
		http.Error(w, "hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		upstreamConn.Close()
		http.Error(w, "hijack failed", http.StatusInternalServerError)
		return
	}

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		clientConn.Close()
		upstreamConn.Close()
		return
	}

	s.rotator.OnSuccess()
	s.stats.OnRequest()
	s.pipe(clientConn, upstreamConn)
}

// pipe 双向转发，任一方向结束即关闭双方收尾。
func (s *ProxyServer) pipe(client, upstream net.Conn) {
	done := make(chan struct{}, 2)
	forward := func(dst, src net.Conn) {
		n, _ := io.Copy(dst, src)
		s.stats.OnBytes(n)
		done <- struct{}{}
	}
	go forward(upstream, client)
	go forward(client, upstream)

	<-done
	client.Close()
	upstream.Close()
	<-done
}

// handleHTTP 处理明文 HTTP 转发。客户端发来的是绝对 URI（GET http://target/path）。
func (s *ProxyServer) handleHTTP(w http.ResponseWriter, r *http.Request) {
	upstream, err := s.rotator.Get()
	if err != nil {
		logUpstreamFail(r, upstream, err)
		http.Error(w, "no upstream proxy available", http.StatusBadGateway)
		return
	}

	transport, err := newTransport(s.cfg.Via, upstream)
	if err != nil {
		logUpstreamFail(r, upstream, err)
		http.Error(w, "upstream transport failed", http.StatusBadGateway)
		return
	}

	outreq := r.Clone(r.Context())
	outreq.RequestURI = ""
	for _, h := range hopByHopHeaders {
		outreq.Header.Del(h)
	}

	resp, err := transport.RoundTrip(outreq)
	if err != nil {
		logUpstreamFail(r, upstream, err)
		s.rotator.MarkFailed()
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	s.rotator.OnSuccess()
	s.stats.OnRequest()

	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	n, err := io.Copy(w, resp.Body)
	s.stats.OnBytes(n)
	if err != nil {
		log.Printf("copy response body: %v", err)
	}
}

// logUpstreamFail 记一行转发失败的原因。客户端那边只看得到 502，上游网关给的
// "460 Proxy Authentication Invalid" 这类关键信号不落到日志里就彻底没了，排障只能靠
// 手工 curl 逐条复现。
//
// 上游只记 host:port：代理 URL 带密码，而日志会落盘（/stats 回显完整 URL 是另一回事，
// 那是内存里的即时快照）。upstream 为空说明连当前代理都没取到。
//
// 数据面每连接一个 goroutine，死代理场景下每个请求都会走到这里，日志量与请求量同阶。
// 这是有意的取舍——真出事时正需要每条都在，为它加限流器是过度设计。
func logUpstreamFail(r *http.Request, upstream string, err error) {
	via := "-"
	if u, perr := url.Parse(upstream); perr == nil && u.Host != "" {
		via = u.Host
	}
	log.Printf("%s %s via %s: %v", r.Method, r.Host, via, err)
}
