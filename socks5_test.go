package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

// 入站 SOCKS5 与单端口嗅探的测试。客户端用 golang.org/x/net/proxy——它已经是依赖
// （dialer.go 拿它做出站），不必再手写一个 socks5 客户端。

// startMixed 起一条走 sniffListener 的入口，返回它的 host:port 和目标服务器。
// setupE2E 做不到这一层：httptest.NewServer 直接把连接喂给 http.Server，绕过了嗅探。
//
// 上游直接塞 listProvider 而不走 file 来源，是为了绕开启动探测——这几条测的是入站协议，
// 探测由 provider 的测试覆盖，掺进来只是多一次网络往返。
func startMixed(t *testing.T, auth [2]string) (string, *httptest.Server) {
	t.Helper()

	target := newTarget(t)
	cfg := &Config{
		Mode:           "cycle",
		ParsedInterval: time.Minute,
		Source:         "file", // 失败策略走"轮换免费"那条，不在测试里额外探测
		UnitPrice:      0.03,
		TestURL:        target.URL,
	}
	cfg.Auth.Username, cfg.Auth.Password = auth[0], auth[1]

	stats := NewStats(cfg.UnitPrice)
	provider := &listProvider{proxies: []string{"socks5://" + startSOCKS5(t, "", "")}}
	handler := NewProxyServer(cfg, NewRotator(cfg, provider, stats), stats)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(newSniffListener(ln, handler.HandleSOCKS5))
	t.Cleanup(func() { srv.Close() })

	return ln.Addr().String(), target
}

func socks5Client(t *testing.T, addr string, auth *proxy.Auth) *http.Client {
	t.Helper()
	d, err := proxy.SOCKS5("tcp", addr, auth, proxy.Direct)
	if err != nil {
		t.Fatalf("socks5 client: %v", err)
	}
	return &http.Client{Transport: &http.Transport{
		DialContext: d.(proxy.ContextDialer).DialContext,
	}}
}

// domainURL 把目标地址里的 127.0.0.1 换成 localhost，好让客户端发 ATYP=DOMAIN。
// 走 IP 时客户端已经在本地解析过了（socks5:// 而非 socks5h://），两条分支都要过一遍。
func domainURL(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %s: %v", rawURL, err)
	}
	return "http://localhost:" + u.Port()
}

func TestSOCKS5Inbound(t *testing.T) {
	addr, target := startMixed(t, [2]string{})
	c := socks5Client(t, addr, nil)

	if got := mustGetThrough(t, c, domainURL(t, target.URL)); got != "hello from target" {
		t.Fatalf("ATYP=DOMAIN 响应 = %q", got)
	}
	if got := mustGetThrough(t, c, target.URL); got != "hello from target" {
		t.Fatalf("ATYP=IPv4 响应 = %q", got)
	}
}

func TestSOCKS5InboundAuth(t *testing.T) {
	addr, target := startMixed(t, [2]string{"user", "pass"})

	ok := socks5Client(t, addr, &proxy.Auth{User: "user", Password: "pass"})
	if got := mustGetThrough(t, ok, target.URL); got != "hello from target" {
		t.Fatalf("正确账号响应 = %q", got)
	}

	bad := socks5Client(t, addr, &proxy.Auth{User: "user", Password: "wrong"})
	if _, err := bad.Get(target.URL); err == nil {
		t.Fatal("错误密码应当被拒")
	}
}

// 没配 auth 时客户端只协商 0x02，必须收到 0xFF 而不是"选 0x02 再无条件放行"。
// 回 0x02 是宣告"我要验密码"，不验就是对协议撒谎。
//
// 这一条只能手写握手：x/net/proxy 配了账号时发的是 [0x00, 0x02]，服务端挑 0x00 就过了，
// 测不到这个分支。
func TestSOCKS5InboundRejectsUserPassWhenAuthUnset(t *testing.T) {
	addr, _ := startMixed(t, [2]string{})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{socks5Version, 1, methodUserPass}); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatalf("read method reply: %v", err)
	}
	if resp[1] != methodNoAcceptable {
		t.Fatalf("没配 auth 时不该接受 0x02，收到方法 %#x", resp[1])
	}
}

// 嗅探本身的回归：同一个端口上两种协议都要通。
func TestSniffMixedOnSamePort(t *testing.T) {
	addr, target := startMixed(t, [2]string{})

	proxyURL, _ := url.Parse("http://" + addr)
	httpClient := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	if got := mustGetThrough(t, httpClient, target.URL); got != "hello from target" {
		t.Fatalf("HTTP 侧响应 = %q", got)
	}

	if got := mustGetThrough(t, socks5Client(t, addr, nil), target.URL); got != "hello from target" {
		t.Fatalf("SOCKS5 侧响应 = %q", got)
	}
}
