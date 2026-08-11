package main

import (
	"bufio"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
)

// 本文件是测试用的上游代理实现。真实的短效代理不可控（会过期、会被墙），
// 端到端验证必须有一个本机的、行为确定的上游。

// startHTTPProxy 起一个支持 CONNECT 与明文转发的 http 上游代理，返回 host:port。
// user 非空时要求 Proxy-Authorization，失败回 407——短效 IP 最典型的死法。
func startHTTPProxy(t *testing.T, user, pass string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user != "" {
			want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
			if r.Header.Get("Proxy-Authorization") != want {
				w.Header().Set("Proxy-Authenticate", `Basic realm="fake"`)
				http.Error(w, "", http.StatusProxyAuthRequired)
				return
			}
		}
		if r.Method == http.MethodConnect {
			fakeConnect(w, r)
			return
		}
		outreq := r.Clone(r.Context())
		outreq.RequestURI = ""
		outreq.Header.Del("Proxy-Authorization")
		resp, err := http.DefaultTransport.RoundTrip(outreq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

func fakeConnect(w http.ResponseWriter, r *http.Request) {
	target, err := net.Dial("tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	client, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		target.Close()
		return
	}
	client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() { io.Copy(target, client); target.Close() }()
	go func() { io.Copy(client, target); client.Close() }()
}

// countingRelay 在 addr 前面架一层只计数的 TCP 中转，用来断言某一跳确实被走到了。
// 链式代理的测试少了它就没有意义：前置漏传时连接会直奔上游，照样成功。
func countingRelay(t *testing.T, addr string) (string, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			go func() {
				up, err := net.Dial("tcp", addr)
				if err != nil {
					c.Close()
					return
				}
				go func() { io.Copy(up, c); up.Close() }()
				io.Copy(c, up)
				c.Close()
			}()
		}
	}()
	return ln.Addr().String(), &n
}

// startSOCKS5 起一个只支持 CONNECT 的 socks5 上游，返回 host:port。
// user 非空时要求 username/password 认证（RFC 1929）。
func startSOCKS5(t *testing.T, user, pass string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveSOCKS5(c, user, pass)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String()
}

func serveSOCKS5(c net.Conn, user, pass string) {
	defer c.Close()
	br := bufio.NewReader(c)

	// 握手：VER NMETHODS METHODS...
	head := make([]byte, 2)
	if _, err := io.ReadFull(br, head); err != nil {
		return
	}
	if _, err := io.ReadFull(br, make([]byte, head[1])); err != nil {
		return
	}
	if user == "" {
		c.Write([]byte{5, 0})
	} else {
		c.Write([]byte{5, 2})
		if !socks5Auth(br, c, user, pass) {
			return
		}
	}

	// 请求：VER CMD RSV ATYP DST.ADDR DST.PORT
	req := make([]byte, 4)
	if _, err := io.ReadFull(br, req); err != nil {
		return
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		io.ReadFull(br, b)
		host = net.IP(b).String()
	case 3:
		l := make([]byte, 1)
		io.ReadFull(br, l)
		b := make([]byte, l[0])
		io.ReadFull(br, b)
		host = string(b)
	case 4:
		b := make([]byte, 16)
		io.ReadFull(br, b)
		host = net.IP(b).String()
	default:
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(br, pb); err != nil {
		return
	}
	addr := net.JoinHostPort(host, strconv.Itoa(int(pb[0])<<8|int(pb[1])))

	target, err := net.Dial("tcp", addr)
	if err != nil {
		c.Write([]byte{5, 1, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer target.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})

	go io.Copy(target, br) // 用 br：握手时可能已经预读了隧道的头几个字节
	io.Copy(c, target)
}

func socks5Auth(br *bufio.Reader, c net.Conn, user, pass string) bool {
	h := make([]byte, 2) // VER ULEN
	if _, err := io.ReadFull(br, h); err != nil {
		return false
	}
	u := make([]byte, h[1])
	io.ReadFull(br, u)
	pl := make([]byte, 1)
	io.ReadFull(br, pl)
	p := make([]byte, pl[0])
	io.ReadFull(br, p)

	if string(u) != user || string(p) != pass {
		c.Write([]byte{1, 1})
		return false
	}
	c.Write([]byte{1, 0})
	return true
}
