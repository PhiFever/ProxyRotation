package main

import (
	"encoding/base64"
	"io"
	"log"
	"net"
	"net/http"
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
		http.Error(w, "no upstream proxy available", http.StatusBadGateway)
		return
	}

	upstreamConn, err := dialUpstream(s.cfg.Via, upstream, r.Host)
	if err != nil {
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
		http.Error(w, "no upstream proxy available", http.StatusBadGateway)
		return
	}

	transport, err := newTransport(s.cfg.Via, upstream)
	if err != nil {
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
