package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// CommandProvider 的对外契约：脚本只管往 stdout 打一行代理，拨号协议与代理账号由配置补齐。
// examples/ 下的取 IP 脚本都按这个契约写，改坏了它们会静默拿到一个拼错的 URL。
func TestCommandProviderBuildsURL(t *testing.T) {
	cases := []struct {
		name    string
		command string
		user    string
		want    string
	}{
		{
			name:    "裸 host:port 按 proxy_scheme + 代理账号拼",
			command: "echo 1.2.3.4:8080",
			user:    "u",
			want:    "socks5://u:p@1.2.3.4:8080",
		},
		{
			name:    "没配代理账号则不拼账号",
			command: "echo 1.2.3.4:8080",
			want:    "socks5://1.2.3.4:8080",
		},
		{
			name:    "自带 scheme 的原样返回，不套第二层账号",
			command: "echo http://a:b@1.2.3.4:8080",
			user:    "u",
			want:    "http://a:b@1.2.3.4:8080",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &CommandProvider{cfg: &Config{
				Command:     c.command,
				ProxyScheme: "socks5",
				ProxyUser:   c.user,
				ProxyPass:   "p",
			}}
			got, err := p.Next()
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if got != c.want {
				t.Fatalf("Next() = %q, want %q", got, c.want)
			}
		})
	}
}

// 脚本用非 0 退出码报错（取 IP 接口出错时正文是 "ERROR(-3): ..." 而 HTTP 状态仍是 200，
// 只能由脚本自己判断后退出）。这里退化成 nil error 的话，会拿错误信息当代理去拨号。
func TestCommandProviderFailsOnNonZeroExit(t *testing.T) {
	p := &CommandProvider{cfg: &Config{Command: "echo boom >&2; exit 1", ProxyScheme: "socks5"}}
	if got, err := p.Next(); err == nil {
		t.Fatalf("脚本退出码非 0 却返回了 %q", got)
	}
}

func newTestAPIProvider(apiURL, user string) *APIProvider {
	return &APIProvider{
		cfg: &Config{
			APIURL:      apiURL,
			ProxyScheme: "socks5",
			ProxyUser:   user,
			ProxyPass:   "p",
		},
		client: &http.Client{Timeout: providerTimeout},
	}
}

// APIProvider 与 CommandProvider 共用 buildProxyURL，组装规则那一半已由上面的
// command 用例覆盖；这里只是再确认 api 走的确实是同一条路（有人复制出第二份实现就会红），
// 重点在 api 独有的那一半：HTTP 取回 + 正文取行。
func TestAPIProviderBuildsURL(t *testing.T) {
	cases := []struct {
		name string
		body string
		user string
		want string
	}{
		{
			name: "裸 host:port 按 proxy_scheme + 代理账号拼",
			body: "1.2.3.4:8080\n",
			user: "u",
			want: "socks5://u:p@1.2.3.4:8080",
		},
		{
			name: "自带 scheme 的原样返回，不套第二层账号",
			body: "http://a:b@1.2.3.4:8080\n",
			user: "u",
			want: "http://a:b@1.2.3.4:8080",
		},
		{
			// 取 IP 接口常见的两种脏输出：前面空一行、行尾是 CRLF。
			// \r 混进 URL 会变成一个拨不通的主机名，而且报错信息里看不出来。
			name: "跳过前导空行，行尾 CR 不进 URL",
			body: "\r\n  \r\n1.2.3.4:8080\r\n",
			want: "socks5://1.2.3.4:8080",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, c.body)
			}))
			defer srv.Close()

			got, err := newTestAPIProvider(srv.URL, c.user).Next()
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if got != c.want {
				t.Fatalf("Next() = %q, want %q", got, c.want)
			}
		})
	}
}

// 接口出错的两种表象都不能退化成"拿到了一个代理"：拿错误正文去拨号会把一次
// 取 IP 失败伪装成一个死上游，触发本不该发生的轮换（api 来源每换一次都要花钱）。
func TestAPIProviderRejectsBadResponse(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{name: "非 200", status: http.StatusForbidden, body: "quota exhausted"},
		{name: "200 但正文为空", status: http.StatusOK, body: ""},
		{name: "200 但只有空白行", status: http.StatusOK, body: "\n  \n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				fmt.Fprint(w, c.body)
			}))
			defer srv.Close()

			if got, err := newTestAPIProvider(srv.URL, "").Next(); err == nil {
				t.Fatalf("接口返回 %d/%q 却拿到了代理 %q", c.status, c.body, got)
			}
		})
	}
}

// 重试是给取 IP 接口的瞬时抖动兜底的：一次 5xx 不该让整条链停摆。
func TestAPIProviderRetriesThenSucceeds(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) < providerRetries {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, "1.2.3.4:8080")
	}))
	defer srv.Close()

	got, err := newTestAPIProvider(srv.URL, "").Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if want := "socks5://1.2.3.4:8080"; got != want {
		t.Fatalf("Next() = %q, want %q", got, want)
	}
	if n := hits.Load(); n != providerRetries {
		t.Fatalf("请求了 %d 次，want %d", n, providerRetries)
	}
}
