package main

import "testing"

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
