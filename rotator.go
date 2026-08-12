package main

import (
	"log"
	"sync"
	"time"
)

const (
	defaultCooldown      = 5 * time.Second
	defaultFailThreshold = 3

	// probeTimeout 是单次探测的上限（dialer.go 的 probeProxy 用）。
	// probeCacheTTL 是探测结果的缓存期，两者的关系有硬约束：
	//
	//	probeCacheTTL > probeTimeout
	//
	// 否则缓存刚写入就过期，失败场景下等于没有缓存，每次失败都要重新探一遍。
	// 上限同样有约束：缓存绝不能设成分钟级——代理刚死时会命中旧的"有效"缓存，
	// 反而拒绝该做的切换（原版缓存 60s 就是这么砸的）。
	probeTimeout  = 3 * time.Second
	probeCacheTTL = 5 * time.Second
)

type Rotator struct {
	provider Provider
	source   string // file | api | command，决定失败路径策略
	mode     string // cycle | loadbalance
	interval time.Duration
	testURL  string

	mu         sync.RWMutex
	current    string
	lastSwitch time.Time
	forceNext  bool
	cooldown   time.Duration

	failThreshold    int
	consecutiveFails int

	// proven 记录当前代理有没有至少成功转发过一次。轮换时它还是 false，说明这个
	// 上游从拿到手到被换掉一次都没通过——按个收费的来源下，那就是白买了一个。
	// deadIPs 数的正是连续白买了几个，任何一次成功即清零。
	proven     bool
	deadIPs    int
	maxDeadIPs int

	// onExhausted 在 deadIPs 触到上限时调用，默认实现打一行日志并退出进程。
	// 在写锁内调用，实现里不要再回调 Rotator。
	onExhausted func(deadIPs int)

	lastProbe   time.Time
	lastProbeOK bool

	// probe 恒为绑好前置代理的 probeProxy，独立成字段只为让 MarkFailed 能在没有真
	// 代理的情况下被测。构造后不再改动，因此读它不需要持锁。
	probe func(proxyURL, testURL string) bool

	stats *Stats
}

func NewRotator(cfg *Config, p Provider, s *Stats) *Rotator {
	return &Rotator{
		provider:      p,
		source:        cfg.Source,
		mode:          cfg.Mode,
		interval:      cfg.ParsedInterval,
		testURL:       cfg.TestURL,
		cooldown:      defaultCooldown,
		failThreshold: defaultFailThreshold,
		maxDeadIPs:    cfg.MaxDeadIPs,
		// 停机而不是继续降级：数据采集这类无人值守的消费者需要的是"任务失败"这个信号。
		// 拿着一个死代理继续跑，下游会收到一堆空数据却以为采集正常，而按个收费的来源
		// 还会一个接一个地买新 IP。
		//
		// 不用 panic：net/http 对 handler goroutine 的 panic 有 recover，只会断掉一条
		// 连接、进程照跑，而 Get / MarkFailed 全是从 handler goroutine 调过来的。
		onExhausted: func(deadIPs int) {
			log.Fatalf("连续 %d 个上游一次都没跑通，停机。请检查 via / 代理账号 / test_url 可达性", deadIPs)
		},
		probe: func(proxyURL, testURL string) bool {
			return probeProxy(cfg.Via, proxyURL, testURL)
		},
		stats: s,
	}
}

// Get 由数据面按请求调用。cycle 模式的切换判断就发生在这里，不用后台定时器——
// 没有流量就不获取新 IP，因为短效 IP 按个收费，空闲时主动拉新 IP 就是烧钱。
func (r *Rotator) Get() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case r.current == "": // 首次获取
	case r.forceNext: // 失败自愈
		r.forceNext = false
	case r.mode == "loadbalance": // 每请求一换
	case r.interval > 0 && time.Since(r.lastSwitch) >= r.interval: // 到点了
	default:
		return r.current, nil
	}
	return r.rotateLocked()
}

// Switch 由管理接口 POST /switch 调用，立即强制轮换。
func (r *Rotator) Switch() (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rotateLocked()
}

func (r *Rotator) Current() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.current
}

// OnSuccess 在转发成功时调用，清零连续失败计数。
func (r *Rotator) OnSuccess() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.consecutiveFails = 0
	r.proven = true
	r.deadIPs = 0
}

// MarkFailed 在上游失败时调用。是否切换取决于来源的成本模型：
// 只有 file 来源轮换免费，api / command 每换一次都要买一个付费 IP 或跑一次取 IP 脚本。
func (r *Rotator) MarkFailed() {
	r.mu.Lock()

	if time.Since(r.lastSwitch) < r.cooldown {
		r.mu.Unlock()
		return // 防抖：刚换过就别在抖动期反复切
	}

	if r.source == "file" {
		// 轮换免费：计数即换，不探测。探测在这里省不下钱，纯属多一次网络往返。
		r.consecutiveFails++
		if r.consecutiveFails >= r.failThreshold {
			r.forceNext = true
			r.consecutiveFails = 0
		}
		r.mu.Unlock()
		return
	}

	// api / command：先确认当前代理是不是真的死了。
	// 探通 → 是目标/网络抖动的锅，保留当前代理，省下一个付费 IP。
	if time.Since(r.lastProbe) < probeCacheTTL {
		if !r.lastProbeOK {
			r.forceNext = true
		}
		r.mu.Unlock()
		return
	}
	probed := r.current
	r.mu.Unlock()

	// 探测必须在锁外：它要通过一个可能已经挂掉的代理发真实请求，耗时接近
	// probeTimeout（死代理通常是挂起到超时，不是快速 RST）。持锁做这件事会让
	// 所有 Get() 一起冻结——而故障恢复期恰恰是最该保持可用的时候。
	// 探测期间 Get() 继续返回当前代理，这也正符合"它也许还活着"的判断前提。
	alive := r.probe(probed, r.testURL)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastProbe, r.lastProbeOK = time.Now(), alive
	// 复查：探测期间 current 可能已被 /switch 或到期轮换换掉，旧结论对新代理无效。
	// 少了这一行，就会因为"P1 死了"而切掉从没测过的 P2，白烧一个付费 IP——
	// 正是探测本来要省下的那个。
	if !alive && r.current == probed {
		r.forceNext = true
	}
}

// rotateLocked 必须在写锁内调用。
// provider.Next() 失败时保留旧 current：单次抖动不该中断服务。但这种降级有上界，
// 由下面的 deadIPs 兜住——取不到新 IP 时 proven 同样留在 false，于是反复尝试恢复
// 而始终不成功的情况最终也会停机，不会永远拿着一个死代理装作还在工作。
func (r *Rotator) rotateLocked() (string, error) {
	// 先结上一个上游的账，再买下一个：已经触顶就不该再掏钱。
	if r.current != "" && !r.proven {
		r.deadIPs++
		if r.maxDeadIPs > 0 && r.deadIPs >= r.maxDeadIPs {
			r.onExhausted(r.deadIPs)
		}
	}

	next, err := r.provider.Next()
	if err != nil {
		if r.current != "" {
			return r.current, nil
		}
		return "", err
	}
	r.current = next
	r.lastSwitch = time.Now()
	r.consecutiveFails = 0
	r.proven = false
	r.stats.OnSwitch(next)
	return next, nil
}
