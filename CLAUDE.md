# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

ProxyRotation：把只存活 1–60 分钟的"短效代理 IP"聚合成一个固定不变的本地代理入口（HTTP 与 SOCKS5 共用一个端口），自动轮换上游、并统计用了多少 IP / 花了多少钱。它是 Python 版 [ProxyCat](https://github.com/honmashironeko/ProxyCat) 的**从 0 重写的简化版**，不是移植——因此产物一律叫 ProxyRotation，文中出现 ProxyCat 时指的都是那个 Python 原版。

模块路径 `github.com/PhiFever/ProxyRotation`（计划公开，GPL-3.0）。v1 是**扁平 main 包**，没有 `cmd/` 或 `internal/`。

## 当前状态

v1 链路已全部实现并通过验收：两种上游 scheme（socks5 / http）、两条数据面路径（CONNECT 与明文）、三种来源（file / api / command）、链式代理 `via`、启动探测、失败自愈、消费闸、管理接口。**入站 SOCKS5 原本排在 M3，已提前实现。** 下一步见 README 路线图（M2：文件池后台健康检查、Transport 连接池复用）。

有两处只有单测证据、真机从未跑过，**写文档时别说成"验证过"**：

- `source=api` 的 `fetch()`（`file` 与 `command` 都跑过真机）。组装逻辑与 `command` 共用 `buildProxyURL`，且有 `httptest` 假接口的单测覆盖，真机没覆盖的只剩那十行取数。
- `responseHeaderTimeout` 的触发路径。唯一对端是测试里的 `startBlackhole`（只 Accept、不读不回）。

**持久记录只有本文件与 `README.md`**：新增的设计结论必须写进这两处之一。

## 常用命令

```bash
go mod tidy                      # 初始化/同步依赖
go build -o proxyrotation .      # 单静态二进制（扁平 main 包，注意是 "." 不是 ./cmd/...）
go run . -c config.yaml          # 直接跑；-version 打印版本后退出
go vet ./... && gofmt -l .

go test ./...                    # 全部测试
go test -race ./...              # 锁外探测那段改动后必跑
go test -run TestRotatorCycle -v # 单个测试
```

测试不碰外网：`fakeproxy_test.go` 里有一个 http 上游代理和一个 socks5 服务端，是所有转发测试的对端。**别把测试改成打真实代理**——短效代理随时过期，那样的测试今天绿明天红。要验证真机链路请用下面的验收命令手工跑。

常量之间的硬约束由不变式测试守着，调参时它们会先红：`TestProbeCacheTTLInvariant`（`probeCacheTTL > probeTimeout`）、`TestResponseHeaderTimeoutInvariant`（`responseHeaderTimeout > probeTimeout`）。

验收（三条都通过才算达标）：

```bash
curl -x http://localhost:1080 https://ifconfig.me   # 应返回上游代理出口 IP，不是本机 IP
curl -s localhost:8080/stats                        # 请求数/轮换数/unique_ips/成本
curl -sX POST localhost:8080/switch                 # 强制立刻换上游
```

## 架构要点

三条执行流通过共享的 `Rotator` / `Stats` 解耦，互不直接调用：

1. **数据面**：`proxy.go` 裸 `net/http`，每连接一个 goroutine → `Rotator.Get()` 拿当前上游 → `dialUpstream()` 打通 → 双向 `io.Copy`。
2. **轮换控制**：`Rotator` 持有"当前代理"，由数据面按需驱动，**没有后台定时器**。
3. **管理面**：`admin.go` 用 gin 在独立端口读 `Stats` 快照 / 调 `Rotator.Switch()`。

关键分层：`Provider`（file / api / command 三种代理来源，只回答"下一个代理是谁"）与 `Rotator`（只回答"什么时候该换"）严格分离。加新来源只实现 `Provider` 接口，不碰轮换逻辑。

### 必须守住的设计决策

这些是重写的核心价值所在，改动前务必想清楚：

- **懒切换**：cycle 模式的切换判断发生在请求到来时，不是定时器。**没有流量就绝不获取新 IP**——短效 IP 按个收费，空转拉 IP 就是烧钱。不要"顺手"改成 `time.Ticker`。
- **失败自愈按来源分流**（成本模型驱动，不是可有可无的优化）：
  - `file` 来源轮换免费 → 连续失败计数达阈值即切，**不探测**（探测省不下钱，纯多一次往返）。
  - `api` / `command` 来源每换一次都有成本 → 失败时**先 `probeProxy` 探测当前代理**，探通说明是目标/网络抖动，保留当前代理不切。
  - 探测结果只缓存**几秒**，且必须满足 `probeCacheTTL > probeTimeout`，否则缓存刚写入就过期、失败场景下等于没有缓存。上限也有约束：原版缓存 60s 是明确的 bug，代理刚死时会命中旧的"有效"缓存，反而拒绝该做的切换。
  - **探测必须在锁外做**，做完重取锁并复查 `r.current == probed`。持写锁探测会冻结所有 `Get()` 长达一个探测超时，而故障恢复期正是最该保持可用的时候；不复查则会因"P1 死了"切掉探测期间换上来的、从没测过的 P2，白烧一个付费 IP。这一条是对最初设计（"在写锁内做完判定"）的**有意偏离**，别改回去。
  - 探测走的是「通过当前代理请求 `test_url`」，**不能退化成 TCP 连通性检查**：短效 IP 死在代理商网关的鉴权/计费层（IP 到期、额度耗尽、白名单掉了），这些情况 TCP 全都连得上。真机上见过两种死法——网关快速 `connection refused`，以及**握手 granted、请求发得出去、然后无限挂起**；后者正是 TCP 检查看不见的那种，**别假设只有一种**。
  - `test_url` 必须在**代理出口所在地**可达，否则健康代理被判死、省钱机制静默失效。默认值面向国内出口，改动前先确认可达性。
  - `file` 来源在**启动时**把整张表并发探一遍，死代理不进轮换（`filterAlive`）；全表探不通就拒绝启动。这是探测唯一免费的时机——代理已经在手上了。api / command 做不了这件事：它们每探一个都要先买一个 IP。并发有上限 `probeConcurrency`，不设的话几千行的表会一次开几千个 fd，撞 ulimit 后健康代理会被 EMFILE 误杀。
- **`POST /switch` 有幂等窗口**（复用 `cooldown`，5s）。它的用法是"目标站点把这个 IP 标记了，换一个"——而风控验证页是**正常的 200 响应**，代理层无从分辨，所以检测只能在调用方，`/switch` 是它唯一的执行手段。三条约束：
  - 并发 worker 会同时撞上同一个被标记的 IP、各打一次 `/switch`。没有窗口的话，实测 12 个并发调用**买了 13 个 IP**，且这些切换之间没有成功转发穿插（`proven` 一直是 false），`deadIPs` 累加到 11、消费闸踩响 9 次——防烧钱的闸门被正常用法踩响。回归保护是 `TestConcurrentSwitchDoesNotTripDeadIPs`。
  - 窗口用**独立的 `lastForced`**，不能复用 `lastSwitch`：后者被首次获取、到期轮换、失败自愈一起更新，拿它当窗口会把"刚拿到一个 IP 就发现它被标记了"这类合法的立即切换一并挡掉，而那正是 `/switch` 最该管用的时刻（这个坑踩过，e2e 与单测同时变红）。
  - 返回的 `switched` 区分"换好了，可以重试"和"没换成，该退避"，后者含窗口内命中与 `provider.Next()` 失败降级两种。别把它简化掉——调用方拿不到这个信号就只能盲目重试。
- **上游拨号只认 `socks5` 和 `http`**（`dialUpstream` 与 `newTransport` 两条路径的 scheme 分派必须保持一致）。故意不支持 `https` 上游：配置校验与 README 都只承诺这两种，多支持一种就得同步改三处文档。
- **链式代理（`via`）是拨号层的事，不进轮换层**：`chainDialer` 把 `via` 和上游按顺序叠成一个拨号器，每一跳的"怎么连到我自己"就是上一跳。因此 `rotator.go` / `provider.go` / `stats.go` 完全不知道 `via` 存在，计费口径也自动正确——前置是固定基础设施，不该计入按个收费的 `unique_ips`。三条约束：
  - 链上每一跳的类型是本地的 `dialer` 接口（`Dial` + `DialContext`）。**`connectDialer` 必须实现 `Dial`**，虽然从没被调用过：`proxy.SOCKS5` 的 `forward` 形参类型是 `proxy.Dialer`，只实现 `DialContext` 时下一跳是 socks5 就传不进去。别以"死代码"为由删掉它。
  - `newTransport` 里最后一跳是 `http` 时，那一跳交给 `t.Proxy` 而不是链（`hops[:len-1]` 才进 `chainDialer`）——Transport 设了 `Proxy` 后 `DialContext` 收到的正是代理自己的地址，两段刚好接上。最后一跳是 socks5 时整条链都归 `DialContext`。
  - **前置挂掉必须 fail fast**（`main.go` 里启动时单独探一次）。否则每个上游都会"看起来"是死的：file 来源整表探不通拒绝启动还算安全，api / command 来源会每次失败都判定该换 IP，正好以最快速度烧钱——恰是失败自愈本来要防的事。不要改成"运行中自动降级为直连"，那会静默绕过用户配前置的理由。
- **`newTransport` 的 `DisableKeepAlives` 与"每请求新建 Transport"是绑定的**：v1 每个请求造一个新 Transport，不关长连接就会每请求在池里留一条永不复用也永不回收的空闲连接。M2 改成按上游 URL 缓存 Transport 时，这两件事必须一起改。
- **单端口嗅探（HTTP + SOCKS5）靠首字节判别**：SOCKS5 开口必是 `0x05`，HTTP 代理请求首字节必是方法名首字母（全 ASCII 大写字母），两者不相交。SOCKS4（`0x04`）不单独处理，落进 HTTP 分支由 `net/http` 回 400。四条约束：
  - **peek 绝不能写在 `sniffListener.Accept()` 或 accept 循环里。** 它是阻塞 I/O，一条连上却不发字节的连接会卡住整个 accept 循环，后面所有新连接排队等它超时——一行位置之差就是拒绝服务。正确形状是「accept 循环 goroutine → 每连接再开 goroutine 做 peek」。
  - 关的是 `done` 而不是 `conns`：`dispatch` 可能正阻塞在往 `conns` 发送，关 channel 会让它 panic。
  - **配了 `auth` 时 SOCKS5 侧必须回 `0x02` 并真的验密码。** HTTP 侧的门在 `checkInboundAuth`，是另一条代码路径，漏接不会有任何报错，而 `listen` 默认全网卡——那就是给一个自认为设了密码的端口开免密后门。反过来没配 `auth` 时只接受 `0x00`，不接受 `0x02` 后无条件放行：方法协商是**双向承诺**，回 `0x02` 等于宣告"我要验密码"，不验就是撒谎。这与 HTTP 侧"没配 auth 就不看 `Proxy-Authorization`"的宽松行为不同，但那不是不一致——`Proxy-Authorization` 是单向请求头，忽略它不构成承诺。回归保护是 `TestSOCKS5InboundRejectsUserPassWhenAuthUnset`。
  - `ATYP=DOMAIN` 的域名**原样交给上游解析**，与 CONNECT 路径传 `r.Host` 同一口径。客户端发 IP 说明它配的是 `socks5://` 而非 `socks5h://`，DNS 已在它那边泄露且解析结果是本机地区的，我们无从补救，只能在 README 提醒。
  - 只实现 `CONNECT`：它与 `dialUpstream` 返回的 TCP 隧道一一对应。REP 码失败一律 `0x01`，与"失败一律回 502"同口径——错误是"经上游拨号失败"，本就分不清目标不可达还是上游死了。
- **`Hijack()` 返回的 `*bufio.ReadWriter` 不能丢**（`proxy.go`）：`net/http` 读请求头用的是它自己的 bufio，客户端若把 `CONNECT` 与紧随其后的 TLS ClientHello 塞进同一个 TCP 段，那几个字节已在缓冲区里，交出裸 conn 就等于永久丢掉——隧道建好却握不上手。用 `bufferedConn` 把它接回读取路径。出站侧的对称检查在 `connectHandshake` 的 `br.Buffered() > 0`。
- **gin 不碰数据面**：转发路径是裸 `net/http` + `io.Copy`。gin 只承载 `/stats` `/switch` `/healthz`，且用 `gin.New()` 而非 `gin.Default()`。
- **失败的请求一律对客户端返回 502**，自愈只决定"下一个请求用哪个代理"，不做请求级重试。五个 502 分支统一走 `logUpstreamFail`，格式是 `方法 目标 via 上游host:port: 原因`。**上游只记 host:port**——代理 URL 带密码而日志会落盘，取舍和 `/stats` 回显完整 `current_proxy` 不同（后者是内存里的即时快照）。死代理场景下日志量与请求量同阶，这是有意的：真出事时正需要每条都在，加限流器是过度设计。
- **`responseHeaderTimeout`（10s）补的是 `dialTimeout` 管不到的那一段**：上游接了 TCP、请求也发得出去、然后永不回话时，没有它明文请求会一直挂到客户端自己放弃，期间既不返回 502 也不触发 `MarkFailed`——失败自愈叫不醒。取值两头都有约束：必须 > `probeTimeout`（`probeProxy` 也走 `newTransport`，小于它会把探测的超时口径悄悄改掉），又要容得下"上游正常但慢"。**它只管到响应头——响应体传到一半卡住仍然没有上限，v1 有意不做。**
- **`max_dead_ips` 是无人值守时唯一的消费闸**（默认 10，负数不限）：连续这么多个上游一次都没成功转发过就停机，任何一次成功即清零。计数发生在 `rotateLocked` 换掉旧上游时，靠 `proven` 标志判断"这个 IP 从拿到手到被换掉有没有通过一次"，所以同一个死 IP 反复判死只算一次。三条约束：
  - **触顶要在 `provider.Next()` 之前判**，否则会在停机前多买一个。
  - **不能用 `panic`**：`net/http` 对 handler goroutine 的 panic 有 recover，只会断掉一条连接、进程照跑，而 `Get` / `MarkFailed` 全是从 handler goroutine 调过来的。默认实现是 `log.Fatalf`（退出码 1）；它在写锁内被调用，替换实现时不要回调 Rotator。
  - `provider.Next()` 失败时保留旧 current 的降级**必须受同一个计数约束**（那时 `proven` 同样留在 false）。这条降级本身是对的——单次抖动不该中断服务——但没有上界就会变成"拿着一个死代理无限期假装在工作"，而无人值守的消费者（数据采集）需要的恰恰是"任务失败"这个信号。
- **成本口径**：`estimated_cost = len(uniqueIPs) × unit_price`，`uniqueIPs` 按代理 URL 的 host 部分去重（同一 IP 存活期内重复选中不重复计费）。

### 两个容易踩的坑

- **两类鉴权别混淆**（原版最大的坑）：调 `api_url` 拉代理这个动作的鉴权（token/appKey/签名）一律塞进 URL 查询参数，**没有独立配置字段**；而 `proxy_user` / `proxy_pass` 是拿到代理后用它连目标时的账号，用于把裸 `ip:port` 拼成 `scheme://user:pass@ip:port`。
- **`loadbalance` + `api`/`command` 是成本陷阱**：每请求换一个上游 = 每请求买一个付费 IP / 跑一次取 IP 脚本。`loadbalance` 只推荐配 `file` 来源，api/command 用 `cycle`。文档和配置注释里都要写明。

### source=command 的定位

`CommandProvider` 执行用户脚本、读 stdout 第一行，组装规则与 `APIProvider` **完全一致**（含 `://` 原样返回，裸 `host:port` 按 `proxy_scheme` + 账号拼）——这两段逻辑必须复用同一个函数（`buildProxyURL`），不要复制。它的存在是为了把"时间签名""先登记本机 IP 再取 IP"这类各家私有的多步流程赶出核心，是 Python 版"人人自改 `getip.py`"在编译型二进制下的等价物。**任何代理商专属逻辑都不进核心代码。**

**脚本能吐完整 URL 就别用 `proxy_scheme` / `proxy_user` / `proxy_pass`**：那三项是给"接口只吐裸 ip:port"兜底的，能省则省——每一项都是一次配错的机会。`examples/kuaidaili.sh` 就是范例：取 IP 时让账号随 IP 一起返回（无额外往返），脚本补上 scheme 前缀后直接输出完整 URL，配置因此只剩四行。scheme 写死在脚本里也比留成配置项安全——"这家必须走 socks5"这类结论就没法被配错了。

## 与 Python 原版的关系

原版可作行为参考，但以下是**明确不继承的历史包袱**，从原版找思路时先对照这张表，别无意识照搬：

- 双入口（`ProxyCat.py` + `app.py`）+ 从未实例化的 `ProxyCat` 死类 → 单入口 `main.go`
- 三条转发路径里复制粘贴的重试块 → 失败即 `MarkFailed()`，逻辑只写一遍
- `handle_proxy_failure→check_current_proxy→switch_proxy` 里 6 个锁/冷却标志交织 → 保留探测能力，砍掉缠绕实现
- 345 行 `MESSAGES` 中英字典 → 不做 i18n
- ini 分段配置 + 单账号与 `[Users]` 并存 → 单一扁平 YAML
- 内存日志 handler、tqdm 进度条、版本检测联网、赞助 banner → 全部砍掉，统计交给 `/stats`
- 版本号在两个文件硬编码 → 单一 `const Version`，`-version` 输出

v1 的非目标同样明确：Web UI、多用户/IP 黑白名单、i18n、热重载都不做（路线图见 `README.md`）。收到这类需求先确认是不是要提前做，而不是直接加。
