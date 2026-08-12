package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeProvider 每次返回一个新代理，用序号区分。
type fakeProvider struct {
	mu sync.Mutex
	n  int
}

func (p *fakeProvider) Next() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	return fmt.Sprintf("socks5://10.0.0.%d:1080", p.n), nil
}

func newTestRotator(source, mode string, interval time.Duration) *Rotator {
	cfg := &Config{
		Source:         source,
		Mode:           mode,
		ParsedInterval: interval,
		TestURL:        "http://test.invalid",
	}
	r := NewRotator(cfg, &fakeProvider{}, NewStats(0.01))
	// 防抖冷却在测试里必须关掉：它会让刚轮换后的 MarkFailed 直接被吞掉。
	r.cooldown = 0
	r.probe = func(string, string) bool { return false }
	return r
}

func mustGet(t *testing.T, r *Rotator) string {
	t.Helper()
	p, err := r.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return p
}

// cycle 模式只在到点时换：没到点的请求必须复用当前代理，否则就是在烧钱。
func TestRotatorCycle(t *testing.T) {
	r := newTestRotator("file", "cycle", time.Minute)

	p1 := mustGet(t, r)
	if got := mustGet(t, r); got != p1 {
		t.Fatalf("未到点就轮换了：%s -> %s", p1, got)
	}

	r.lastSwitch = time.Now().Add(-time.Minute)
	if got := mustGet(t, r); got == p1 {
		t.Fatalf("到点了却没轮换，仍是 %s", got)
	}
}

func TestRotatorLoadbalance(t *testing.T) {
	r := newTestRotator("file", "loadbalance", 0)

	seen := map[string]bool{}
	for range 3 {
		seen[mustGet(t, r)] = true
	}
	if len(seen) != 3 {
		t.Fatalf("loadbalance 应当每请求一换，实际只用了 %d 个代理", len(seen))
	}
}

// file 来源轮换免费：连续失败计数到阈值即换，不探测。
func TestMarkFailedFileCountsToThreshold(t *testing.T) {
	r := newTestRotator("file", "cycle", time.Minute)
	r.probe = func(string, string) bool {
		t.Fatal("file 来源不该探测：探测省不下钱，纯属多一次往返")
		return false
	}

	p1 := mustGet(t, r)
	for range defaultFailThreshold - 1 {
		r.MarkFailed()
	}
	if got := mustGet(t, r); got != p1 {
		t.Fatalf("没到阈值就换了：%s -> %s", p1, got)
	}

	r.MarkFailed()
	if got := mustGet(t, r); got == p1 {
		t.Fatalf("到阈值了却没换，仍是 %s", got)
	}
}

// OnSuccess 清零计数：零星失败混着成功不该攒够阈值。
func TestOnSuccessResetsFailures(t *testing.T) {
	r := newTestRotator("file", "cycle", time.Minute)

	p1 := mustGet(t, r)
	for range defaultFailThreshold * 2 {
		r.MarkFailed()
		r.OnSuccess()
	}
	if got := mustGet(t, r); got != p1 {
		t.Fatalf("成功已清零计数，不该轮换：%s -> %s", p1, got)
	}
}

// api 来源探通 → 是目标/网络抖动，保留当前代理，省下一个付费 IP。
func TestMarkFailedAPIKeepsProxyWhenProbeAlive(t *testing.T) {
	r := newTestRotator("api", "cycle", time.Minute)
	r.probe = func(string, string) bool { return true }

	p1 := mustGet(t, r)
	r.MarkFailed()
	if got := mustGet(t, r); got != p1 {
		t.Fatalf("代理还活着却换掉了，白烧一个付费 IP：%s -> %s", p1, got)
	}
}

// api 来源探不通 → 代理真死了，下一个请求换。
func TestMarkFailedAPISwitchesWhenProbeDead(t *testing.T) {
	r := newTestRotator("api", "cycle", time.Minute)

	p1 := mustGet(t, r)
	r.MarkFailed()
	if got := mustGet(t, r); got == p1 {
		t.Fatalf("代理已死却没换，仍是 %s", got)
	}
}

// 探测在锁外做，期间 current 可能已被换掉。旧结论不能牵连新代理，
// 否则会因为"P1 死了"切掉从没测过的 P2——白烧一个本来要省下的付费 IP。
func TestMarkFailedRecheckAfterProbe(t *testing.T) {
	r := newTestRotator("api", "cycle", time.Minute)

	entered := make(chan struct{})
	release := make(chan struct{})
	r.probe = func(string, string) bool {
		close(entered)
		<-release
		return false // 被探的那个（P1）确实死了
	}

	p1 := mustGet(t, r)

	done := make(chan struct{})
	go func() {
		r.MarkFailed()
		close(done)
	}()

	<-entered // MarkFailed 已放锁、正卡在探测里
	p2, _, err := r.Switch()
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if p2 == p1 {
		t.Fatalf("Switch 没换代理")
	}
	close(release)
	<-done

	if got := mustGet(t, r); got != p2 {
		t.Fatalf("P1 的死讯牵连了没测过的 P2：want %s, got %s", p2, got)
	}
}

// 探测期间 Get() 不能被阻塞：故障恢复期恰恰是最该保持可用的时候。
func TestGetNotBlockedDuringProbe(t *testing.T) {
	r := newTestRotator("api", "cycle", time.Minute)

	entered := make(chan struct{})
	release := make(chan struct{})
	r.probe = func(string, string) bool {
		close(entered)
		<-release
		return false
	}

	p1 := mustGet(t, r)
	go r.MarkFailed()
	<-entered

	got := make(chan string, 1)
	go func() { got <- mustGet(t, r) }()

	select {
	case p := <-got:
		if p != p1 {
			t.Fatalf("探测期间应继续返回当前代理，got %s", p)
		}
	case <-time.After(time.Second):
		t.Fatal("Get() 被探测阻塞了")
	}
	close(release)
}

// 连续买到的上游一个都没跑通 → 停机。无人值守时这是唯一的消费闸：继续降级下去，
// 按个收费的来源会一个接一个买新 IP，而下游采集任务只会收到一堆空数据还以为一切正常。
func TestRotatorStopsAfterConsecutiveDeadIPs(t *testing.T) {
	r := newTestRotator("api", "cycle", time.Minute)
	r.maxDeadIPs = 3
	var got int
	r.onExhausted = func(deadIPs int) { got = deadIPs }

	// 每次都是"拿到新 IP → 失败 → 探测判死"，一个都没成功转发过
	for range r.maxDeadIPs {
		mustGet(t, r)
		r.MarkFailed()
	}
	if got != 0 {
		t.Fatalf("还没触顶就停机了（deadIPs=%d）", got)
	}

	mustGet(t, r) // 第 3 个死 IP 在这里被结账，触顶
	if got != r.maxDeadIPs {
		t.Fatalf("触顶没有停机：onExhausted 收到 %d, want %d", got, r.maxDeadIPs)
	}
}

// 中间只要成功转发过一次，计数就清零：真正危险的是"连续"全死，
// 偶发失败混着成功说明链路还活着，不该把服务停掉。
func TestSuccessClearsDeadIPs(t *testing.T) {
	r := newTestRotator("api", "cycle", time.Minute)
	r.maxDeadIPs = 2
	r.onExhausted = func(int) { t.Error("中间成功过，不该停机") }

	for range r.maxDeadIPs * 3 {
		mustGet(t, r)
		r.OnSuccess()
		r.MarkFailed()
	}
}

// 取不到新 IP 时保留旧代理是有意的降级，但它必须有上界——否则程序会拿着一个
// 死代理无限期地假装在工作，这正是数据采集任务最怕的失败方式。
func TestDeadIPsCountsFailedProviderToo(t *testing.T) {
	r := newTestRotator("api", "cycle", time.Minute)
	r.maxDeadIPs = 2
	stopped := false
	r.onExhausted = func(int) { stopped = true }

	mustGet(t, r) // 先拿到一个上游，之后 provider 再也取不出新的
	r.provider = &brokenProvider{}

	for i := 0; i < r.maxDeadIPs && !stopped; i++ {
		r.MarkFailed()
		mustGet(t, r) // 降级：仍返回旧代理
	}
	if !stopped {
		t.Fatal("provider 一直失败却没有停机")
	}
}

type brokenProvider struct{}

func (brokenProvider) Next() (string, error) { return "", fmt.Errorf("取不到 IP") }

// /switch 的用法是"目标站点把这个 IP 标记了，换一个"，并发采集里 N 个 worker 会同时
// 撞上同一个被标记的 IP 各打一次。窗口内只该买一个 IP，其余调用拿到同一个新代理。
func TestSwitchIsIdempotentWithinCooldown(t *testing.T) {
	r := newTestRotator("command", "cycle", time.Minute)
	r.cooldown = time.Minute // newTestRotator 默认关掉了冷却，这里正是要测它

	p1 := mustGet(t, r)

	p2, switched, err := r.Switch()
	if err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if !switched || p2 == p1 {
		t.Fatalf("第一次 switch 没换成：switched=%v, %s -> %s", switched, p1, p2)
	}

	for range 5 {
		got, switched, err := r.Switch()
		if err != nil {
			t.Fatalf("Switch: %v", err)
		}
		if switched {
			t.Fatal("窗口内又买了一个 IP")
		}
		if got != p2 {
			t.Fatalf("窗口内返回的不是刚换好的那个：want %s, got %s", p2, got)
		}
	}

	r.lastForced = time.Now().Add(-2 * time.Minute) // 窗口过去
	if p3, switched, _ := r.Switch(); !switched || p3 == p2 {
		t.Fatalf("窗口过后仍然换不动：switched=%v, %s -> %s", switched, p2, p3)
	}
}

// 没有窗口的话，并发 switch 之间没有成功转发穿插，proven 一直是 false，
// deadIPs 会一路累加到停机——防烧钱的闸门被正常用法踩响。
func TestConcurrentSwitchDoesNotTripDeadIPs(t *testing.T) {
	r := newTestRotator("command", "cycle", time.Minute)
	r.cooldown = time.Minute
	r.maxDeadIPs = 3
	r.onExhausted = func(deadIPs int) { t.Errorf("并发 switch 把消费闸踩响了，deadIPs=%d", deadIPs) }

	mustGet(t, r)
	r.OnSuccess()

	var wg sync.WaitGroup
	for range r.maxDeadIPs * 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := r.Switch(); err != nil {
				t.Errorf("Switch: %v", err)
			}
		}()
	}
	wg.Wait()

	if n := r.stats.Snapshot().UniqueIPs; n != 2 {
		t.Fatalf("一轮并发 switch 买了 %d 个 IP，want 2（初始 1 个 + 换 1 次）", n)
	}
}

// probeCacheTTL > probeTimeout 是硬约束：反过来的话缓存刚写入就过期，
// 失败场景下等于没有缓存。上限也有约束，分钟级缓存会拒绝该做的切换。
func TestProbeCacheTTLInvariant(t *testing.T) {
	if probeCacheTTL <= probeTimeout {
		t.Fatalf("probeCacheTTL(%v) 必须大于 probeTimeout(%v)", probeCacheTTL, probeTimeout)
	}
	if probeCacheTTL >= time.Minute {
		t.Fatalf("probeCacheTTL(%v) 不能到分钟级：代理刚死时会命中旧的\"有效\"缓存", probeCacheTTL)
	}
}
