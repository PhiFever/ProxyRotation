package main

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 本文件跑的是 CLAUDE.md 里那三条验收命令的等价物，只是把不可控的真实短效代理
// 换成了本机的假上游：curl -x 走代理拿目标内容 / GET /stats / POST /switch。

type e2eEnv struct {
	client   *http.Client // 已配置成走本程序的代理
	admin    http.Handler
	stats    *Stats
	target   *httptest.Server // 明文目标
	tlsTgt   *httptest.Server // https 目标，走 CONNECT
	proxyURL string
}

func setupE2E(t *testing.T, via string, upstreams []string, auth [2]string) *e2eEnv {
	t.Helper()

	target := newTarget(t)
	tlsTgt := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello over tls")
	}))
	t.Cleanup(tlsTgt.Close)

	listFile := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(listFile, []byte(joinLines(upstreams)), 0o600); err != nil {
		t.Fatalf("write proxy list: %v", err)
	}

	cfg := &Config{
		Mode:           "cycle",
		ParsedInterval: time.Minute,
		Source:         "file",
		ProxyFile:      listFile,
		Via:            via,
		UnitPrice:      0.03,
		TestURL:        target.URL, // 启动探测打这里
	}
	cfg.Auth.Username, cfg.Auth.Password = auth[0], auth[1]

	provider, err := NewProvider(cfg) // 顺带验证启动探测不会误杀健康代理
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	stats := NewStats(cfg.UnitPrice)
	rotator := NewRotator(cfg, provider, stats)

	front := httptest.NewServer(NewProxyServer(cfg, rotator, stats))
	t.Cleanup(front.Close)

	frontURL, _ := url.Parse(front.URL)
	if auth[0] != "" {
		frontURL.User = url.UserPassword(auth[0], auth[1])
	}
	return &e2eEnv{
		client: &http.Client{Transport: &http.Transport{
			Proxy:           http.ProxyURL(frontURL),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}},
		admin:    NewAdminHandler(rotator, stats),
		stats:    stats,
		target:   target,
		tlsTgt:   tlsTgt,
		proxyURL: front.URL,
	}
}

func joinLines(ss []string) string {
	out := ""
	for _, s := range ss {
		out += s + "\n"
	}
	return out
}

func (e *e2eEnv) mustGet(t *testing.T, rawURL string) string {
	t.Helper()
	return mustGetThrough(t, e.client, rawURL)
}

func mustGetThrough(t *testing.T, c *http.Client, rawURL string) string {
	t.Helper()
	resp, err := c.Get(rawURL)
	if err != nil {
		t.Fatalf("get %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get %s: 状态 %s", rawURL, resp.Status)
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func (e *e2eEnv) snapshot(t *testing.T) Snapshot {
	t.Helper()
	rec := httptest.NewRecorder()
	e.admin.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/stats 状态 %d", rec.Code)
	}
	var s Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode /stats: %v", err)
	}
	return s
}

// 两条主路径：CONNECT 隧道（https）与明文 HTTP 转发。
func TestE2EForwardsThroughUpstream(t *testing.T) {
	up := "http://" + testUser + ":" + testPass + "@" + startHTTPProxy(t, testUser, testPass)
	e := setupE2E(t, "", []string{up}, [2]string{})

	if got := e.mustGet(t, e.tlsTgt.URL); got != "hello over tls" {
		t.Fatalf("CONNECT 隧道内容不对: %q", got)
	}
	if got := e.mustGet(t, e.target.URL); got != "hello from target" {
		t.Fatalf("明文转发内容不对: %q", got)
	}

	s := e.snapshot(t)
	if s.TotalRequests != 2 {
		t.Fatalf("total_requests = %d, want 2", s.TotalRequests)
	}
	if s.BytesTransferred == 0 {
		t.Fatal("bytes_transferred 没有累加")
	}
	if s.EstimatedCost != 0.03 {
		t.Fatalf("estimated_cost = %v, want 0.03（1 个 IP × 单价）", s.EstimatedCost)
	}
}

func TestE2ESwitchRotatesUpstream(t *testing.T) {
	ups := []string{
		"http://" + testUser + ":" + testPass + "@" + startHTTPProxy(t, testUser, testPass),
		"http://" + testUser + ":" + testPass + "@" + startHTTPProxy(t, testUser, testPass),
	}
	e := setupE2E(t, "", ups, [2]string{})

	e.mustGet(t, e.target.URL)
	before := e.snapshot(t)

	rec := httptest.NewRecorder()
	e.admin.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/switch", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/switch 状态 %d", rec.Code)
	}

	after := e.snapshot(t)
	if after.CurrentProxy == before.CurrentProxy {
		t.Fatalf("/switch 没换上游，仍是 %s", after.CurrentProxy)
	}
	if got := e.mustGet(t, e.target.URL); got != "hello from target" {
		t.Fatalf("换上游后转发坏了: %q", got)
	}
	// 计费按代理 URL 的 host 去重：两个上游都在 127.0.0.1，只算一个 IP。
	if after.UniqueIPs != 1 {
		t.Fatalf("unique_ips = %d, want 1（同一 host 不重复计费）", after.UniqueIPs)
	}
}

// 配了 via 时两条数据面路径都必须真的经过前置代理。dialer 的链路测试查不出这一条：
// 只要有一处忘了把 cfg.Via 传下去，连接会直奔上游、照样成功，链却静默失效了。
// 所以这里不看"能不能通"，而是数前置被穿过几次。
func TestE2EThroughFrontProxy(t *testing.T) {
	frontAddr, hits := countingRelay(t, startSOCKS5(t, testUser, testPass))
	front := "socks5://" + testUser + ":" + testPass + "@" + frontAddr
	up := "http://" + testUser + ":" + testPass + "@" + startHTTPProxy(t, testUser, testPass)
	e := setupE2E(t, front, []string{up}, [2]string{})

	baseline := hits.Load() // 启动探测已经走过一遍前置

	if got := e.mustGet(t, e.tlsTgt.URL); got != "hello over tls" { // CONNECT
		t.Fatalf("链式 CONNECT 内容不对: %q", got)
	}
	if got := e.mustGet(t, e.target.URL); got != "hello from target" { // 明文
		t.Fatalf("链式明文转发内容不对: %q", got)
	}

	if n := hits.Load() - baseline; n != 2 {
		t.Fatalf("经过前置的连接数 = %d, want 2（两条数据面路径各一次）", n)
	}
}

// 上游连不通时一律回 502，不做请求级重试。
func TestE2EReturnsBadGatewayOnDeadUpstream(t *testing.T) {
	cfg := &Config{Mode: "cycle", ParsedInterval: time.Minute, Source: "file", TestURL: "http://test.invalid"}
	stats := NewStats(0)
	rotator := NewRotator(cfg, &deadProvider{}, stats)

	front := httptest.NewServer(NewProxyServer(cfg, rotator, stats))
	defer front.Close()
	frontURL, _ := url.Parse(front.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(frontURL)}}

	resp, err := client.Get("http://example.invalid/")
	if err != nil {
		t.Fatalf("请求应当拿到 502 而不是传输层错误: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态 = %s, want 502", resp.Status)
	}
}

type deadProvider struct{}

// 127.0.0.1:1 保证连不通，且不依赖外网
func (deadProvider) Next() (string, error) { return "http://127.0.0.1:1", nil }

// listProvider 按顺序吐出预设的代理，用完停在最后一个。
type listProvider struct {
	proxies []string
	n       int
}

func (p *listProvider) Next() (string, error) {
	proxy := p.proxies[p.n]
	if p.n < len(p.proxies)-1 {
		p.n++
	}
	return proxy, nil
}

// 上游接了 TCP 却永不回话时，明文路径必须在响应超时内回 502 并叫醒失败自愈。
// 没有 ResponseHeaderTimeout 时这里会一直挂到客户端自己放弃（实测 25s+），
// 期间既不返回 502 也不触发 MarkFailed——省钱机制在最该起作用的时候失效。
func TestE2EResponseHeaderTimeoutTriggersFailover(t *testing.T) {
	orig := responseHeaderTimeout
	responseHeaderTimeout = 200 * time.Millisecond
	t.Cleanup(func() { responseHeaderTimeout = orig })

	target := newTarget(t)
	good := "http://" + testUser + ":" + testPass + "@" + startHTTPProxy(t, testUser, testPass)
	provider := &listProvider{proxies: []string{"http://" + startBlackhole(t), good}}

	cfg := &Config{Mode: "cycle", ParsedInterval: time.Minute, Source: "file", TestURL: target.URL}
	stats := NewStats(0)
	rotator := NewRotator(cfg, provider, stats)
	rotator.cooldown = 0      // 防抖会吞掉刚轮换后的 MarkFailed
	rotator.failThreshold = 1 // 一次失败就换，免得为凑阈值多等两个超时

	front := httptest.NewServer(NewProxyServer(cfg, rotator, stats))
	defer front.Close()
	frontURL, _ := url.Parse(front.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(frontURL)}}

	start := time.Now()
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("请求应当拿到 502 而不是传输层错误: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态 = %s, want 502", resp.Status)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("挂了 %v 才回 502：响应超时没生效", elapsed)
	}

	// MarkFailed 真的被叫醒了：下一个请求应该已经换到活着的上游。
	if got := mustGetThrough(t, client, target.URL); got != "hello from target" {
		t.Fatalf("超时后没有切换上游: %q", got)
	}
}

func TestE2EInboundAuth(t *testing.T) {
	up := "http://" + testUser + ":" + testPass + "@" + startHTTPProxy(t, testUser, testPass)
	e := setupE2E(t, "", []string{up}, [2]string{"bob", "pw"})

	// 带账号：通
	if got := e.mustGet(t, e.target.URL); got != "hello from target" {
		t.Fatalf("带账号应当放行: %q", got)
	}

	// 不带账号：407
	frontURL, _ := url.Parse(e.proxyURL)
	naked := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(frontURL)}}
	resp, err := naked.Get(e.target.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("无账号状态 = %s, want 407", resp.Status)
	}
}

// 全表都探不通时启动就该失败，而不是带着一池死代理跑起来。
func TestFileProviderRejectsAllDeadList(t *testing.T) {
	listFile := filepath.Join(t.TempDir(), "proxies.txt")
	os.WriteFile(listFile, []byte("http://127.0.0.1:1\nhttp://127.0.0.1:2\n"), 0o600)

	_, err := NewFileProvider("", listFile, "http://test.invalid")
	if err == nil {
		t.Fatal("全是死代理却启动成功了")
	}
}

// 启动探测只剔除死的，活的必须留下。
func TestFileProviderDropsDeadOnly(t *testing.T) {
	target := newTarget(t)
	good := "http://" + testUser + ":" + testPass + "@" + startHTTPProxy(t, testUser, testPass)

	listFile := filepath.Join(t.TempDir(), "proxies.txt")
	os.WriteFile(listFile, []byte("http://127.0.0.1:1\n"+good+"\n# 注释\n"), 0o600)

	p, err := NewFileProvider("", listFile, target.URL)
	if err != nil {
		t.Fatalf("NewFileProvider: %v", err)
	}
	next, err := p.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if next != good {
		t.Fatalf("轮换里混进了死代理: %s", next)
	}
}
