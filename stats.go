package main

import (
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

type Stats struct {
	start     time.Time
	requests  atomic.Int64
	switches  atomic.Int64
	bytes     atomic.Int64
	unitPrice float64

	mu        sync.Mutex
	uniqueIPs map[string]struct{}
	current   string
}

type Snapshot struct {
	UptimeSeconds    int64   `json:"uptime_seconds"`
	CurrentProxy     string  `json:"current_proxy"`
	TotalRequests    int64   `json:"total_requests"`
	TotalSwitches    int64   `json:"total_switches"`
	UniqueIPs        int     `json:"unique_ips"`
	BytesTransferred int64   `json:"bytes_transferred"`
	UnitPrice        float64 `json:"unit_price"`
	EstimatedCost    float64 `json:"estimated_cost"`
}

func NewStats(unitPrice float64) *Stats {
	return &Stats{
		start:     time.Now(),
		unitPrice: unitPrice,
		uniqueIPs: make(map[string]struct{}),
	}
}

func (s *Stats) OnRequest() { s.requests.Add(1) }

func (s *Stats) OnBytes(n int64) { s.bytes.Add(n) }

func (s *Stats) OnSwitch(proxyURL string) {
	s.switches.Add(1)
	host := proxyHost(proxyURL)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.current = proxyURL
	if host != "" {
		s.uniqueIPs[host] = struct{}{}
	}
}

func (s *Stats) Snapshot() Snapshot {
	s.mu.Lock()
	unique := len(s.uniqueIPs)
	current := s.current
	s.mu.Unlock()

	return Snapshot{
		UptimeSeconds:    int64(time.Since(s.start).Seconds()),
		CurrentProxy:     current,
		TotalRequests:    s.requests.Load(),
		TotalSwitches:    s.switches.Load(),
		UniqueIPs:        unique,
		BytesTransferred: s.bytes.Load(),
		UnitPrice:        s.unitPrice,
		EstimatedCost:    float64(unique) * s.unitPrice,
	}
}

// proxyHost 取代理 URL 的 host 部分作为计费去重键：
// 同一个 IP 在其存活期内被重复选中不重复计费。
func proxyHost(proxyURL string) string {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
