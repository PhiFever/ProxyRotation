package main

import (
	"fmt"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultListen = ":1080"
	// 管理接口默认只监听回环：它无鉴权，且会回显含账号的当前代理 URL
	defaultAdminListen = "127.0.0.1:8080"
	defaultProxyScheme = "socks5"
	// 默认探测目标必须在部署地可达：从国内出口访问 gstatic 之类的地址会失败，
	// 于是健康代理也被判死，失败自愈退化成"每次失败都换 IP"，省钱机制静默失效。
	// 最佳做法是填代理商自己的测试端点，见 config.example.yaml。
	defaultTestURL = "http://connectivitycheck.platform.hicloud.com/generate_204"
	// 连续这么多个上游都没成功转发过一次就停机。取值要能穿过一次短暂抖动，
	// 又不至于在真出事时买下一长串死 IP。
	defaultMaxDeadIPs = 10
)

type Config struct {
	Listen      string `yaml:"listen"`
	AdminListen string `yaml:"admin_listen"`

	Mode     string `yaml:"mode"`
	Interval string `yaml:"interval"`

	Source    string `yaml:"source"`
	ProxyFile string `yaml:"proxy_file"`
	APIURL    string `yaml:"api_url"`
	Command   string `yaml:"command"`

	ProxyScheme string `yaml:"proxy_scheme"`
	ProxyUser   string `yaml:"proxy_user"`
	ProxyPass   string `yaml:"proxy_pass"`

	// Via 是可选的前置代理：所有上游连接先穿过它。留空则直连上游。
	Via string `yaml:"via"`

	UnitPrice float64 `yaml:"unit_price"`
	TestURL   string  `yaml:"test_url"`

	// MaxDeadIPs 是"连续多少个上游一次都没跑通就停机"的上限。留空取默认值，
	// 负数表示不限。见 Rotator.onExhausted：这是无人值守时唯一的消费闸。
	MaxDeadIPs int `yaml:"max_dead_ips"`

	Auth struct {
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"auth"`

	// ParsedInterval 由 Interval 解析而来（yaml.v3 不直接解析 time.Duration）
	ParsedInterval time.Duration `yaml:"-"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.AdminListen == "" {
		c.AdminListen = defaultAdminListen
	}
	if c.ProxyScheme == "" {
		c.ProxyScheme = defaultProxyScheme
	}
	if c.TestURL == "" {
		c.TestURL = defaultTestURL
	}
	if c.MaxDeadIPs == 0 {
		c.MaxDeadIPs = defaultMaxDeadIPs
	}
}

func (c *Config) validate() error {
	switch c.Mode {
	case "cycle", "loadbalance":
	default:
		return fmt.Errorf("mode must be cycle or loadbalance, got %q", c.Mode)
	}

	switch c.ProxyScheme {
	case "socks5", "http":
	default:
		return fmt.Errorf("proxy_scheme must be socks5 or http, got %q", c.ProxyScheme)
	}

	if c.Via != "" {
		u, err := url.Parse(c.Via)
		if err != nil {
			return fmt.Errorf("via: %w", err)
		}
		switch u.Scheme {
		case "socks5", "http":
		default:
			return fmt.Errorf("via scheme must be socks5 or http, got %q", u.Scheme)
		}
	}

	switch c.Source {
	case "file":
		if c.ProxyFile == "" {
			return fmt.Errorf("source=file requires proxy_file")
		}
		info, err := os.Stat(c.ProxyFile)
		if err != nil {
			return fmt.Errorf("proxy_file: %w", err)
		}
		if info.Size() == 0 {
			return fmt.Errorf("proxy_file %s is empty", c.ProxyFile)
		}
	case "api":
		if c.APIURL == "" {
			return fmt.Errorf("source=api requires api_url")
		}
	case "command":
		if c.Command == "" {
			return fmt.Errorf("source=command requires command")
		}
	default:
		return fmt.Errorf("source must be file, api or command, got %q", c.Source)
	}

	if c.Mode == "cycle" {
		d, err := time.ParseDuration(c.Interval)
		if err != nil {
			return fmt.Errorf("interval: %w", err)
		}
		c.ParsedInterval = d
	}

	return nil
}
