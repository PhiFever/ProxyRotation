package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	providerRetries = 3
	providerTimeout = 10 * time.Second
	// 启动探测的并发上限。不设上限的话，几千行的代理表会一次性开几千个 fd，
	// 撞上 ulimit 后大批探测因 EMFILE 失败、健康代理被当成死的剔掉。
	probeConcurrency = 64
)

// Provider 只负责"给下一个代理"，不关心何时该切——切换时机是 Rotator 的职责。
type Provider interface {
	Next() (string, error)
}

func NewProvider(cfg *Config) (Provider, error) {
	switch cfg.Source {
	case "file":
		return NewFileProvider(cfg.Via, cfg.ProxyFile, cfg.TestURL)
	case "api":
		return &APIProvider{cfg: cfg, client: &http.Client{Timeout: providerTimeout}}, nil
	case "command":
		return &CommandProvider{cfg: cfg}, nil
	default:
		return nil, fmt.Errorf("unknown source %q", cfg.Source)
	}
}

// buildProxyURL 把来源返回的一行文本组装成完整代理 URL。
// api 与 command 两个来源共用这一套规则——不要复制第二份实现。
func buildProxyURL(line string, cfg *Config) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", errors.New("empty proxy line")
	}
	// 已含 scheme：来源自带完整 URL（甚至自带账号），原样返回
	if strings.Contains(line, "://") {
		return line, nil
	}
	// 裸 host:port：按 proxy_scheme + 配置里的代理账号拼
	if cfg.ProxyUser != "" {
		return fmt.Sprintf("%s://%s:%s@%s", cfg.ProxyScheme, cfg.ProxyUser, cfg.ProxyPass, line), nil
	}
	return cfg.ProxyScheme + "://" + line, nil
}

func firstNonEmptyLine(r io.Reader) (string, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			return line, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", errors.New("no non-empty line in response")
}

// ---------- file ----------

type FileProvider struct {
	mu      sync.Mutex
	proxies []string
	idx     int
}

// NewFileProvider 启动时把整张表探测一遍，死代理不进轮换。
// 只有 file 来源做得起这件事：它的代理是现成的，探测不额外花钱；
// api / command 来源每探一个都要先买一个 IP，所以它们只在失败时按需探测。
func NewFileProvider(via, path, testURL string) (*FileProvider, error) {
	proxies, err := readProxyFile(path)
	if err != nil {
		return nil, err
	}
	if len(proxies) == 0 {
		return nil, fmt.Errorf("no usable proxy in %s", path)
	}

	alive := filterAlive(via, proxies, testURL)
	if len(alive) == 0 {
		return nil, fmt.Errorf("all %d proxies in %s failed the startup probe (check test_url reachability from the proxy egress)", len(proxies), path)
	}
	if dead := len(proxies) - len(alive); dead > 0 {
		log.Printf("file provider: %d/%d proxies dead at startup, rotating over the remaining %d", dead, len(proxies), len(alive))
	}
	return &FileProvider{proxies: alive}, nil
}

// filterAlive 并发探测并保持原有顺序。
func filterAlive(via string, proxies []string, testURL string) []string {
	ok := make([]bool, len(proxies))
	sem := make(chan struct{}, probeConcurrency)

	var wg sync.WaitGroup
	for i, p := range proxies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ok[i] = probeProxy(via, p, testURL)
		}()
	}
	wg.Wait()

	alive := make([]string, 0, len(proxies))
	for i, p := range proxies {
		if ok[i] {
			alive = append(alive, p)
		}
	}
	return alive
}

func (p *FileProvider) Next() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.proxies) == 0 {
		return "", errors.New("proxy list is empty")
	}
	proxy := p.proxies[p.idx]
	p.idx = (p.idx + 1) % len(p.proxies)
	return proxy, nil
}

func readProxyFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var proxies []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		proxies = append(proxies, line)
	}
	return proxies, scanner.Err()
}

// ---------- api ----------

type APIProvider struct {
	cfg    *Config
	client *http.Client
}

func (p *APIProvider) Next() (string, error) {
	var lastErr error
	for range providerRetries {
		line, err := p.fetch()
		if err != nil {
			lastErr = err
			continue
		}
		return buildProxyURL(line, p.cfg)
	}
	return "", fmt.Errorf("api provider: %w", lastErr)
}

func (p *APIProvider) fetch() (string, error) {
	resp, err := p.client.Get(p.cfg.APIURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("api returned %s", resp.Status)
	}
	return firstNonEmptyLine(resp.Body)
}

// ---------- command ----------

// CommandProvider 把塞不进静态 URL 的动态逻辑（时间签名、白名单登记等）
// 赶进用户自己的脚本，核心保持通用。任何代理商专属逻辑都不进本仓库。
type CommandProvider struct {
	cfg *Config
}

func (p *CommandProvider) Next() (string, error) {
	var lastErr error
	for range providerRetries {
		line, err := p.run()
		if err != nil {
			lastErr = err
			continue
		}
		return buildProxyURL(line, p.cfg)
	}
	return "", fmt.Errorf("command provider: %w", lastErr)
}

func (p *CommandProvider) run() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), providerTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "sh", "-c", p.cfg.Command).Output()
	if err != nil {
		return "", err
	}
	return firstNonEmptyLine(bytes.NewReader(out))
}
