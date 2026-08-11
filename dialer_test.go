package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const (
	testUser = "alice"
	testPass = "s3cr3t"
)

// newTarget 起一个被访问的目标站点，返回其 URL。
func newTarget(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from target")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// getThroughTunnel 在已打通的隧道上发一次 HTTP 请求，验证隧道真的可读写。
func getThroughTunnel(t *testing.T, conn net.Conn, rawURL string) string {
	t.Helper()
	defer conn.Close()

	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if err := req.Write(conn); err != nil {
		t.Fatalf("write over tunnel: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read over tunnel: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func TestDialUpstreamHTTPConnect(t *testing.T) {
	target := newTarget(t)
	upstream := "http://" + testUser + ":" + testPass + "@" + startHTTPProxy(t, testUser, testPass)

	conn, err := dialUpstream("", upstream, hostPort(t, target.URL))
	if err != nil {
		t.Fatalf("dialUpstream: %v", err)
	}
	if got := getThroughTunnel(t, conn, target.URL); got != "hello from target" {
		t.Fatalf("隧道内容不对: %q", got)
	}
}

func TestDialUpstreamSOCKS5(t *testing.T) {
	target := newTarget(t)
	upstream := "socks5://" + testUser + ":" + testPass + "@" + startSOCKS5(t, testUser, testPass)

	conn, err := dialUpstream("", upstream, hostPort(t, target.URL))
	if err != nil {
		t.Fatalf("dialUpstream: %v", err)
	}
	if got := getThroughTunnel(t, conn, target.URL); got != "hello from target" {
		t.Fatalf("隧道内容不对: %q", got)
	}
}

// 代理账号被拒 → CONNECT 收 407。短效 IP 到期/额度耗尽就长这样，
// 必须报成错误而不是把一条没打通的 conn 交出去。
func TestDialUpstreamRejectsBadCredentials(t *testing.T) {
	target := newTarget(t)
	upstream := "http://" + testUser + ":wrong@" + startHTTPProxy(t, testUser, testPass)

	conn, err := dialUpstream("", upstream, hostPort(t, target.URL))
	if err == nil {
		conn.Close()
		t.Fatal("账号错误却打通了隧道")
	}
	if !strings.Contains(err.Error(), "407") {
		t.Fatalf("错误里应能看出是 407: %v", err)
	}
}

// 链式代理：via 与上游的 4 种 scheme 组合都要能打通。
// socks5 那两行同时也验证了 connectDialer 必须实现 Dial —— proxy.SOCKS5 的 forward
// 形参类型是 proxy.Dialer，只实现 DialContext 连编译都过不去。
func TestDialUpstreamViaChain(t *testing.T) {
	target := newTarget(t)
	frontHTTP := "http://" + testUser + ":" + testPass + "@" + startHTTPProxy(t, testUser, testPass)
	frontSOCKS := "socks5://" + testUser + ":" + testPass + "@" + startSOCKS5(t, testUser, testPass)
	upHTTP := "http://" + testUser + ":" + testPass + "@" + startHTTPProxy(t, testUser, testPass)
	upSOCKS := "socks5://" + testUser + ":" + testPass + "@" + startSOCKS5(t, testUser, testPass)

	cases := []struct{ name, via, upstream string }{
		{"http → http", frontHTTP, upHTTP},
		{"http → socks5", frontHTTP, upSOCKS},
		{"socks5 → http", frontSOCKS, upHTTP},
		{"socks5 → socks5", frontSOCKS, upSOCKS},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn, err := dialUpstream(c.via, c.upstream, hostPort(t, target.URL))
			if err != nil {
				t.Fatalf("dialUpstream: %v", err)
			}
			if got := getThroughTunnel(t, conn, target.URL); got != "hello from target" {
				t.Fatalf("链路内容不对: %q", got)
			}
		})
	}
}

func TestDialUpstreamUnsupportedScheme(t *testing.T) {
	if _, err := dialUpstream("", "https://127.0.0.1:8080", "example.com:443"); err == nil {
		t.Fatal("https 上游应当被拒绝")
	}
}

func TestNewTransportHTTP(t *testing.T) {
	target := newTarget(t)
	upstream := "http://" + testUser + ":" + testPass + "@" + startHTTPProxy(t, testUser, testPass)
	assertTransportWorks(t, "", upstream, target.URL)
}

func TestNewTransportSOCKS5(t *testing.T) {
	target := newTarget(t)
	upstream := "socks5://" + testUser + ":" + testPass + "@" + startSOCKS5(t, testUser, testPass)
	assertTransportWorks(t, "", upstream, target.URL)
}

// 链式代理下的明文转发路径。最后一跳是 http 时它由 Transport 自己走（t.Proxy），
// 前置几跳走 DialContext——两段拼错就连不通，所以两种最后一跳都要覆盖。
func TestNewTransportViaChain(t *testing.T) {
	target := newTarget(t)
	via := "socks5://" + testUser + ":" + testPass + "@" + startSOCKS5(t, testUser, testPass)

	t.Run("最后一跳 http", func(t *testing.T) {
		up := "http://" + testUser + ":" + testPass + "@" + startHTTPProxy(t, testUser, testPass)
		assertTransportWorks(t, via, up, target.URL)
	})
	t.Run("最后一跳 socks5", func(t *testing.T) {
		up := "socks5://" + testUser + ":" + testPass + "@" + startSOCKS5(t, testUser, testPass)
		assertTransportWorks(t, via, up, target.URL)
	})
}

func assertTransportWorks(t *testing.T, via, upstream, targetURL string) {
	t.Helper()
	tr, err := newTransport(via, upstream)
	if err != nil {
		t.Fatalf("newTransport: %v", err)
	}
	resp, err := (&http.Client{Transport: tr}).Get(targetURL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello from target" {
		t.Fatalf("响应内容不对: %q", body)
	}
}

func TestProbeProxy(t *testing.T) {
	target := newTarget(t)
	good := startHTTPProxy(t, testUser, testPass)
	socks := startSOCKS5(t, testUser, testPass)

	cases := []struct {
		name     string
		upstream string
		want     bool
	}{
		{"http 活着", "http://" + testUser + ":" + testPass + "@" + good, true},
		{"socks5 活着", "socks5://" + testUser + ":" + testPass + "@" + socks, true},
		// 账号被拒：TCP 完全连得通，只有真实请求能发现它死了
		{"账号被拒", "http://" + testUser + ":wrong@" + good, false},
		{"端口不通", "http://127.0.0.1:1", false},
		{"scheme 不支持", "https://" + good, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := probeProxy("", c.upstream, target.URL); got != c.want {
				t.Fatalf("probeProxy = %v, want %v", got, c.want)
			}
		})
	}
}

func hostPort(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Host
}
