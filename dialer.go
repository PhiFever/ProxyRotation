package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/proxy"
)

// dialTimeout 是"连到上游代理并完成握手"的上限。死掉的短效代理通常是挂起到超时，
// 不是快速 RST，没有这个上限请求 goroutine 会一直悬着。配了 via 时这个上限覆盖整条链。
const dialTimeout = 10 * time.Second

// responseHeaderTimeout 是"请求已发出 → 上游回响应头"的上限，只覆盖明文转发路径
// （CONNECT 路径的握手在 connectHandshake 里自带 deadline）。短效 IP 最典型的死法是
// 接了 TCP、握手也过、请求发得出去，然后永不回话：没有这个上限，明文请求会一直挂到
// 客户端自己放弃（实测 25s+），期间既不返回 502 也不触发 MarkFailed——失败自愈叫不醒。
//
// 取值两头都有约束：必须 > probeTimeout（probeProxy 也走 newTransport，小于它会把
// 探测的超时口径悄悄改掉）；也要容得下"上游正常但慢"，把健康代理误判成死的正是
// 省钱机制失效的方式。
//
// 它只管到响应头——响应体传到一半卡住仍然没有上限，v1 有意不做。
//
// 是 var 而非 const，只为让测试能把它调小。
var responseHeaderTimeout = 10 * time.Second

// dialUpstream 通过上游代理，返回一条已经打通到 targetHostPort 的连接。
// 返回的 conn 就是隧道本身，调用方直接双向 io.Copy 即可。
// via 非空时先穿过它再连上游（链式代理）；为空时行为与不带前置完全一致。
func dialUpstream(via, proxyURL, targetHostPort string) (net.Conn, error) {
	hops, err := parseChain(via, proxyURL)
	if err != nil {
		return nil, err
	}
	d, err := chainDialer(hops)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	defer cancel()
	return d.DialContext(ctx, "tcp", targetHostPort)
}

// newTransport 给明文 HTTP 转发路径用，等价表达同一套上游拨号规则。
func newTransport(via, proxyURL string) (*http.Transport, error) {
	hops, err := parseChain(via, proxyURL)
	if err != nil {
		return nil, err
	}
	last := hops[len(hops)-1]

	// v1 每个请求新建一个 Transport，所以必须关掉长连接：否则每请求都会在连接池里
	// 留下一条永不复用、也永不回收的空闲连接。按上游 URL 缓存 Transport 是 M2 的事，
	// 到那时这一行要跟着摘掉。
	t := &http.Transport{
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}

	switch last.Scheme {
	case "socks5":
		// 整条链都由拨号器走完，Transport 拿到的就是通往目标的连接
	case "http":
		// Transport 设了 Proxy 之后，DialContext 收到的是代理自己的地址——前置几跳
		// 正好接在这里；账号也由 Transport 自己从 last.User 取。
		t.Proxy = http.ProxyURL(last)
		hops = hops[:len(hops)-1]
	default:
		return nil, unsupportedScheme(last.Scheme)
	}

	d, err := chainDialer(hops)
	if err != nil {
		return nil, err
	}
	t.DialContext = d.DialContext
	return t, nil
}

// probeProxy 通过当前代理对 testURL 发一次真实请求，判断代理本身是否还活着。
//
// 必须是"通过代理发真实请求"，不能退化成 TCP 连通性检查：短效 IP 极少以端口不通的
// 方式死掉，它们死在代理商网关的鉴权/计费层（IP 到期、额度耗尽、白名单掉了），
// 这些情况 TCP 全都连得上。
//
// 保持无状态：不缓存、不改任何全局状态。缓存由调用方 Rotator 持有，且只缓存几秒。
func probeProxy(via, proxyURL, testURL string) bool {
	transport, err := newTransport(via, proxyURL)
	if err != nil {
		return false
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   probeTimeout,
		// 不跟随跳转：网关的"额度耗尽/未授权"提示常是 302 到一个 200 的页面，
		// 跟过去就把死代理判成活的了。
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get(testURL)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 400
}

// parseChain 把可选的前置代理和上游代理拼成拨号顺序：本机 → via → upstream。
func parseChain(via, upstream string) ([]*url.URL, error) {
	var hops []*url.URL
	for _, raw := range []string{via, upstream} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("parse proxy %q: %w", raw, err)
		}
		hops = append(hops, u)
	}
	if len(hops) == 0 {
		return nil, fmt.Errorf("no upstream proxy to dial")
	}
	return hops, nil
}

// dialer 是链上每一跳的类型。两个方法都要有：实际调用的始终是 DialContext（这样超时
// 才能覆盖握手阶段），但 proxy.SOCKS5 的 forward 形参类型是 proxy.Dialer，
// 缺了 Dial 就没法把上一跳传给 socks5 跳。
type dialer interface {
	proxy.Dialer
	proxy.ContextDialer
}

// chainDialer 按顺序把每一跳叠成拨号器：返回值的 DialContext(target) 会依次穿过
// 所有跳到达 target。hops 为空时就是直连。
func chainDialer(hops []*url.URL) (dialer, error) {
	var d dialer = &net.Dialer{Timeout: dialTimeout}
	for _, u := range hops {
		switch u.Scheme {
		case "socks5":
			next, err := socks5Dialer(u, d)
			if err != nil {
				return nil, err
			}
			d = next
		case "http":
			d = &connectDialer{proxyURL: u, forward: d}
		default:
			return nil, unsupportedScheme(u.Scheme)
		}
	}
	return d, nil
}

// socks5Dialer 按上游 URL 构造 socks5 拨号器，forward 是"怎么连到这个代理自己"。
func socks5Dialer(u *url.URL, forward proxy.Dialer) (dialer, error) {
	var auth *proxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: pass}
	}
	d, err := proxy.SOCKS5("tcp", u.Host, auth, forward)
	if err != nil {
		return nil, err
	}
	cd, ok := d.(dialer)
	if !ok {
		return nil, fmt.Errorf("socks5 dialer for %s has no DialContext", u.Host)
	}
	return cd, nil
}

// connectDialer 把一个 http 代理包成拨号器：DialContext(addr) 得到的是经该代理打通
// 到 addr 的隧道。forward 是"怎么连到这个代理自己"，链式代理就靠它串起来。
type connectDialer struct {
	proxyURL *url.URL
	forward  dialer
}

func (d *connectDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *connectDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := d.forward.DialContext(ctx, network, d.proxyURL.Host)
	if err != nil {
		return nil, err
	}
	if err := connectHandshake(ctx, conn, d.proxyURL, address); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// connectHandshake 在一条已经连到 http 代理的连接上手写 CONNECT 握手，
// 成功返回后该 conn 即为通往 target 的隧道。
func connectHandshake(ctx context.Context, conn net.Conn, u *url.URL, target string) error {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(dialTimeout)
	}
	// 握手期间限时，握完必须清掉——否则整条隧道会在 deadline 到点时被掐断。
	conn.SetDeadline(deadline)
	defer conn.SetDeadline(time.Time{})

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: make(http.Header),
	}
	if u.User != nil {
		pass, _ := u.User.Password()
		raw := u.User.Username() + ":" + pass
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(raw)))
	}
	if err := req.Write(conn); err != nil {
		return err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 407 在这里就是短效 IP 最典型的死法：IP 到期 / 额度耗尽 / 白名单掉了
		return fmt.Errorf("upstream CONNECT %s: %s", target, resp.Status)
	}
	// 隧道建立后交出去的是裸 conn，bufio 里若还压着字节就会永久丢失。
	// 正常 CONNECT 中上游在客户端开口前不会说话，有残留说明对端行为异常。
	if n := br.Buffered(); n > 0 {
		return fmt.Errorf("upstream sent %d unexpected bytes after CONNECT", n)
	}
	return nil
}

func unsupportedScheme(scheme string) error {
	return fmt.Errorf("unsupported upstream scheme %q (want socks5 or http)", scheme)
}
