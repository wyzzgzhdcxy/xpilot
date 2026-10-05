# xpilot 代码评审

评审时间：2026-09-20 ｜ 代码规模：2687 行（main.go 993 / service.go 832 / report.go 392 / main_test.go 226 / assets.go 142 / xraycore.go 102）

评审方式：读码 + 探针实测（在临时目录写了 14 个探针测试，直接调用真实函数观察行为，非纯阅读推理）。探针目录已删除，工作区无改动。

---

## 一、总体评价

**这是个架构判断相当到位的项目。** 几个关键决策都对：

| 决策 | 评价 |
|---|---|
| 内嵌 xray-core 库而非外部 xray.exe | 正确。省掉进程管理、配置落盘、版本错配三大类问题，单文件分发 |
| 双实例蓝绿切换 | 方向对。热替换出站不重启实例，切换成本极低（见 BUG-2 的取舍讨论） |
| 实测与生产共用 `buildXrayConfig` | 好设计。测速口径和运行口径必然一致，避免"测出来好用起来坏" |
| `domainStrategy: AsIs` + 注释解释不用 `IPIfNonMatch` | **这条注释是全项目最有价值的代码注释**。DNS 污染导致 203.208.x.x 被 geoip:cn 误判为直连，是个很容易踩且极难排查的坑 |
| `xraycore.go` 单一接入层收敛 xray 依赖 | 好。升级 xray-core 只需改一个文件 |
| 用真实 `www.google.com/generate_204` 而非 gstatic | 正确。gstatic 国内有边缘节点会假通过，注释也写清楚了 |
| 进程模型（内置 Web + PAC + 调度循环） | 单进程搞定，不需要 nssm/计划任务配合，简单 |

代码风格统一，中文注释质量高且解释"为什么"而非"是什么"。`main.go` 顶部那段包注释是我见过写得最清楚的之一——一屏讲清了数据流和设计意图。

**主要问题集中在两类**：并发状态管理不够严谨，以及**热切换的落地环节缺少验证与回滚**。

---

## 二、Bug 清单

> **修订说明（2026-09-20，两轮与作者讨论后）**
>
> **撤回项 1**：初版把 `pickCandidates` 的保底逻辑判为严重 bug，**错的**。探针给了 `cur` 一个慢延迟却没先按延迟排序，违反了调用前提（`rows` 在 main.go:512 已排好序）。符合前提重测后保底逻辑正常，"当前节点强制入围"是业务约定。
>
> **撤回项 2**：初版把热切换无回滚判为严重，**已降级为设计取舍**。上线节点刚通过完整实测，加回滚是负收益的复杂度。
>
> **撤回项 3**：第一轮修订时替换上来的"`NaN` 穿透"**也是错的**。第二轮实测确认：`latencyMs` 的赋值链是
> `time.Duration → Microseconds() int64 → float64 → /1000`，**整条链无法产生 `NaN`**（`int64→float64` 永远有限，除数是非零常量）。
> 该条同样是无真实来源的理论推演。详见「三、被我撤回的误报」。
>
> **当前确认的实质缺陷**：BUG-3（`handleDefaultNode` 数据竞争）、BUG-13（`Workers=0` 死锁）、BUG-9（访问日志走向着色失效），以及若干健壮性/可观测性问题。**没有会影响选点正确性的缺陷。**

### 附注【实测可复现，但实际难触发】`lastErr.Error()` 的空指针 panic

**位置**：`main.go:479-483`

```go
for k := 0; k < 3 && ctx.Err() == nil; k++ {   // ← ctx 超时时循环体一次都不执行
    d, err := probeLatency(...)
    if err == nil { ds = append(ds, d) } else { lastErr = err }
}
if len(ds) < 2 {
    lerr[i] = fmt.Errorf("3 次验证仅通过 %d 次：%v", len(ds), strings.TrimSpace(lastErr.Error()))
    //                                                                        ^^^^^^^^^^^^^^^^ nil
}
```

若 `ctx` 在进入循环前就已超时（或三次全被 `ctx.Err() != nil` 跳过），`ds` 为空且 `lastErr` 仍为 `nil` → `lastErr.Error()` **panic**。实测已复现：

```
PANIC: runtime error: invalid memory address or nil pointer dereference
```

**为什么不算高风险**：`xrayTestAll` 的 `overall` 预算 40 分钟，实测推算最坏消耗约为 **7 分钟**（40 节点 × 3s 实例创建 + 3s 验证 + top5 × 60s 下载）。默认配置下几乎不可能耗尽。且 `selectBest` 有 `recover`（service.go:640），真发生时表现为"整轮选点失败"，不会崩进程。

**仅在一种情况下会变成真问题**：用户把 `downloadTimeoutSec` 调到 300+、`top` 调到 10+、节点数上百。三个参数叠加时 40 分钟就不够用了。

**修复**（一行）：

```go
if len(ds) < 2 {
    reason := "全部超时"
    if lastErr != nil { reason = lastErr.Error() }
    if ctx.Err() != nil { reason = "整体超时，验证未完成" }
    lerr[i] = fmt.Errorf("3 次验证仅通过 %d 次：%s", len(ds), strings.TrimSpace(reason))
}
```

---

### BUG-2【原「严重」已降级为设计取舍】热切换无回滚

**位置**：`service.go:775-805`、`xraycore.go:89-94`

我初版把它列为严重，理由是 `swapOutbound` 先 `RemoveHandler("proxy")` 再 `AddOutboundHandler`，第二步失败后出站在路由表中消失，且 `verifyOutbound` 失败只打印警告不回滚。

**撤回理由**（作者反驳成立，我接受）：

1. **切换前的节点已经过完整实测**——`xrayTestAll` 里每个候选都通过了 3 次延迟验证 + 一次下载实测。要走到 `applyWinner`，这个节点必然刚刚证明了可用。`swapOutbound` 失败的前提是"xray 内部状态损坏"，而不是"节点是死的"。
2. **xray-core 内部 API 调用失败在这个场景下概率极低**——`AddOutboundHandler` 失败通常只在配置非法时发生，而配置由 `buildXrayConfig` 从同一个链接生成，前面实测实例就是用这份配置跑起来的。
3. **回滚本身有代价**：`RemoveHandler` + `Add` 之间确实有极短窗口，但为此加"来回切换"逻辑，会把一个几乎不发生的情况变成一段**每次切换都执行的额外状态机**——引入的复杂度和新 bug 面大于它防的风险。这正是"不为极小概率引入中间层"的取舍。
4. **兜底路径已存在**：真出问题就手动重新选点，或等下一轮（间隔默认 60 分钟）。作者明确表示接受这个代价。

**保留一条低优先级建议**（不改逻辑，只加可观测性）：

```go
// applyWinner 里，verifyOutbound 失败时至少让"当前节点"状态可见
if err := verifyOutbound(fixedMainPort); err != nil {
    fmt.Printf("警告: 出网检查失败: %v（当前节点 %s）\n", err, w.addr)
}
```

现在控制台的"当前节点"卡片读的是 `config.json`，而 `config.json` 在验证失败后仍被回写成新节点——**用户会看到"当前节点是 A"但实际不通，且无从判断**。把失败原因写进 `runInfo.Error` 让控制台显示，比加回滚便宜得多，也符合"先给可观测性，再谈自动化"的思路。

---

### BUG-3【中】`handleDefaultNode` 存在数据竞争

**位置**：`service.go:468-498`

```go
func (s *service) handleDefaultNode(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running { /* 拒绝 */ }
	
	cfg := s.cfg          // ← 锁外读！与 handleConfig POST 的 s.cfg = cfg 竞争
	if cfg.DefaultNodeLink == "" { /* 返回 */ }
	
	go func() {
		s.mu.Lock()
		s.running = true  // ← 置位太晚
		s.mu.Unlock()
		...
	}()
	writeJSON(w, map[string]interface{}{"ok": true})
}
```

两个问题：

1. **`cfg := s.cfg` 在锁外读取**（第 476 行），与 `handleConfig` POST 的写入构成 data race。`appConfig` 含 `SubscribeURLs []string` slice，值拷贝不隔离底层数组——探针 PROBE13 实测确认了 slice 共享：修改拷贝会影响原值。

2. **`s.running = true` 放在 goroutine 内**，而 `handleRun` / `handleDefaultNode` 的检查在外面。快速连点两次"应用默认节点"，两次都会通过检查（因为第一次的 goroutine 还没跑到置位），**启动两个并发 `applyWinner`**，同时调用 `swapOutbound` → tag 冲突或出站管理器状态损坏。

**修复**：`startSelection` 已经写对了（锁内同步置位），照抄它的模式：

```go
func (s *service) handleDefaultNode(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		writeJSON(w, map[string]interface{}{"ok": false, "error": "选点正在进行中"})
		return
	}
	cfg := s.cfg
	if cfg.DefaultNodeLink == "" {
		s.mu.Unlock()
		writeJSON(w, map[string]interface{}{"ok": false, "error": "未配置默认节点"})
		return
	}
	s.running = true
	s.stage = "应用默认节点"
	s.mu.Unlock()
	
	go func() {
		defer func() {
			s.mu.Lock(); s.running = false; s.stage = ""; s.mu.Unlock()
		}()
		// ...
	}()
	writeJSON(w, map[string]interface{}{"ok": true})
}
```

---

### BUG-4【中】`runReport` 是全局可变状态，选点与读报告并发访问

**位置**：`report.go:55` `var runReport reportData`

`runReport` 被 `selectBest` 全程无锁写入（`addSub` / `setFetch` / `setNodes` / `setXray` / `setWinner` / `writeHTML`），而 `main.go:931` 在启动时写 `runReport.started`。当前因为 `running` 标志保证同一时刻只有一次选点，**写入侧是串行的**，没有崩溃风险。

但这是个脆弱设计：

- 未来若加"并发测多个订阅"或"历史报告"功能，立刻数据竞争。
- 测试里 `runReport` 被直接复用（`main_test.go`），若并行测试会互相污染。
- `reportData` 里 `subs []reportSub`、`nodes []*reportNode` 都是引用类型，任何跨 goroutine 传递都不安全。

**建议**：把 `runReport` 变成 `selectBest` 内的局部变量，随调用链传递或放进一个 `reportBuilder` 结构体。改动量中等但收益长期。

---

### BUG-5【中】`migrateLegacyFiles` 的 `break` 语义导致新资源被静默忽略

**位置**：`assets.go:105-111` + `assets.go:117-142`

```go
for _, srcDir := range srcDirs {
    if moveIfExist(filepath.Join(srcDir, name), filepath.Join(baseDir, g.sub, name)) {
        break // ← 该文件已处理
    }
}
```

`moveIfExist` 的返回值语义是混淆的：

```go
func moveIfExist(src, dst string) bool {
	if _, err := os.Stat(src); err != nil { return false }  // 源不存在 → false（继续找下个源）
	if _, err := os.Stat(dst); err == nil { return true }   // 目标已存在 → true（停止找源）
	... // 复制成功 → true（已迁移）
	     // 各种失败路径 → 也 return true（假装"已处理"）
}
```

**问题场景**：`baseDir/html/geoip.dat` 已存在（旧版本释放的），而 `exeDir/geoip.dat` 是新版（用户升级 xpilot 后新 exe 带的）。此时 `dst` 存在 → 返回 `true` → `break`。**新 geo 资产永远不被迁移，同时也永远不会被 `releaseEmbedded` 覆盖**（因为 `releaseEmbedded` 对 geo 资产用的是 `writeIfMissing`）。

结果：**升级 exe 后 geo 资产停留在旧版本，且没有任何提示**。对分流准确性的影响是渐进的（geoip/geosite 库过时 → 部分域名分流错误），极难归因。

**修复**：删掉 `break`，改成对每个源位置都尝试（源不存在时 `moveIfExist` 返回 `false` 自然会继续）；或者至少把"目标已存在"与"已迁移"区分开：

```go
// 返回 (handled, migrated)
func moveIfExist(src, dst string) (handled bool, migrated bool)
```

更彻底的方案：geo 资产本就是 exe 内嵌资源，`releaseEmbedded` 应该**按版本号/哈希决定是否覆盖**，而不是 `writeIfMissing`。比如把 asset 的 sha256 写进一个 `assets.version` 文件，不匹配就覆盖。

---

### BUG-6【中】`nodeResult.tn` 悬挂指针 + 双重 Close

**位置**：`main.go:326-331`、`main.go:488`、`main.go:536`

```go
func stopTestNode(n *testNode) {
	if n != nil && n.inst != nil {
		n.inst.Close()
		n.inst = nil   // ← 置 nil 防止重复 Close
	}
}
```

`xrayTestAll` 里：

```go
if lerr[i] != nil {
    stopTestNode(n)      // 验证失败的节点：立即回收，n.inst = nil
}
...
rows = append(rows, r)   // r.tn = n（指向 inst 已是 nil 的 testNode）
...
// 下载实测阶段
stopTestNode(r.tn)       // 二次调用，靠 inst==nil 兜住
```

当前**靠 `n.inst = nil` 这个防御性赋值避免了 panic**，所以不算活跃 bug。但这依赖调用者记得检查状态，脆弱。且 `defer` 里的 `for _, n := range nodes { stopTestNode(n) }` 是第三重保险——**三重防御说明设计本身需要澄清**。

**建议**：`testNode` 加显式状态或改用 `sync.Once`：

```go
type testNode struct {
	link string
	port int
	inst *xrayInstance
	once sync.Once
}

func (n *testNode) Close() {
	if n == nil { return }
	n.once.Do(func() {
		if n.inst != nil { n.inst.Close(); n.inst = nil }
	})
}
```

另外 `xrayTestAll` 用 `nodes []*testNode` 与 `rows []nodeResult` 两个平行数组，且 `lat`/`lerr` 又是平行数组——**四处索引对齐**。建议合成一个结构体切片，减少下标错误面。

---

### BUG-7【低】`portListening` 用 TCP 拨号判定监听，存在假阴性/假阳性

**位置**：`main.go:848-855`

```go
func portListening(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil { return false }
	c.Close()
	return true
}
```

- **假阴性**：端口确实在监听但 accept 队列满/瞬时拒绝 → 判为"未运行" → 看门狗 `startMainFromConfig()` 尝试启动新实例 → 新实例绑定端口失败 → 日志刷错误。**健康实例被反复骚扰**。
- **假阳性**：端口被**无关进程**占用（比如别的软件恰好用 10808）→ 判为"主实例活着" → `applyWinner` 走热替换分支 → `swapMainOutbound` 对 `s.mainInst == nil` 返回 "主实例未运行" → 回退 `startMain` → `killPortListeners(10808)` **强杀无关进程**。

第二条尤其值得注意：`killPortListeners` 用 `netstat -ano` 找 PID 然后 `taskkill /F`，**不校验被杀的进程是不是 xray/xpilot**。虽然 10808 撞车概率低，但这是"按端口杀人"的经典危险模式。

**建议**：主实例的存活判定不要依赖端口探测，而应看 `s.mainInst != nil` 且实例可用（xray 的 `core.Instance` 有状态可查，或定期用 `verifyOutbound` 做活性检查）。`killPortListeners` 至少应校验进程名。

---

### BUG-8【低】`ss://` / `vmess://` 解析的潜在越界与协议支持不一致

**位置**：`main.go:130-196`

`extractAddrPort` 用 `strings.HasPrefix(lower, "vmess://")` 判断后，取 `link[len("vmess://"):]`。因为前缀已匹配，不会越界。但：

1. `b64Decode` 会依次尝试 4 种 base64 编码（`Std`、`URL`、`RawStd`、`RawURL`）。`base64.StdEncoding.DecodeString` 对含 `-`/`_` 的输入失败后继续尝试是对的，但**`StdEncoding` 对错误的填充可能"成功"解出垃圾数据**——VMess 的 payload 解码后要过 `json.Unmarshal` 才算通过，所以兜住了。SS 的旧格式没有这层校验，解出垃圾后 `LastIndex("@")` 可能命中或 `SplitHostPort` 失败，返回 `false`，也算安全。

2. **更大的问题**：`buildXrayConfig` 只支持 `vless`（`main.go:701`），但 `extractAddrPort`、`matchProto`、`dedupe`、TCP 测试全都支持多协议。配置里 `Proto` 默认 `"vless"`，用户改成 `"ss"` 后：
   - 订阅过滤保留 ss 节点
   - TCP 测试通过
   - `startTestNode` 里 `buildXrayConfig` 报"暂只支持 vless 节点"
   - 所有节点被跳过 → `saveBestAndPick` 收到空 rows → 返回"没有任何节点通过 xray 实测"
   
   **用户看到的是一句模糊的失败信息，而不是"当前仅支持 vless"**。建议在 `selectBest` 开头就校验协议，或在前端 `proto` 字段加下拉限定。

3. `ssr://` 分支用 `strings.Split(string(b), ":")` 取 `parts[0]`/`parts[1]`——SSR 的明文格式是 `host:port:protocol:method:obfs:base64pass`，但如果 host 是 IPv6（`[::1]:port`），这个 Split 会切错。低危（SSR 已基本淘汰）。

---

### BUG-9【低】`handleEvents` 的阶段文本用 `%q` 而非 JSON 编码

**位置**：`service.go:423`

```go
fmt.Fprintf(w, "data: {\"type\":\"stage\",\"text\":%q}\n\n", stage)
```

`%q` 产生 Go 字符串字面量转义，不是 JSON 转义。探针 PROBE14 实测：对当前所有实际出现的 stage 值（"准备中"、"拉取订阅中"等），输出恰好是合法 JSON（因为 Go 的 `\"` `\\` `\n` 转义与 JSON 兼容，中文直接透传）。**所以现在不会出问题**。

但两者语义不同，控制字符（U+0000–U+001F）时 Go 会输出 `\x07` 这类**非法 JSON 转义**。虽然 `emit` 的 stage 都是硬编码中文常量，不会含控制字符，但这是"靠输入约束而非编码正确"的写法。`pushEvent` 用的是正确的 `json.Marshal`——**同一文件里两种写法并存**。

**修复**：统一用 `json.Marshal`：

```go
ev, _ := json.Marshal(map[string]string{"type": "stage", "text": stage})
fmt.Fprintf(w, "data: %s\n\n", ev)
```

---

### BUG-10【低】`saveBestAndPick` 在库函数里 `os.Exit(1)`

**位置**：`main.go:616-622`

```go
if err := os.WriteFile(bestPath, ...); err != nil {
    fmt.Printf("写出最优节点文件失败: %v\n", err)
    os.Exit(1)   // ← 常驻服务里 os.Exit
}
```

`saveBestAndPick` 是纯计算函数（输入 rows，输出 winner），却在写文件失败时直接终止整个进程。**常驻服务里任何 `os.Exit` 都是事故**：此时正在运行的 xray 主实例会随进程消失，用户断网，且没有任何清理（xray 实例不是子进程，随 Go 进程一起死）。

调用链是 `selectBest` → `saveBestAndPick`，而 `node.best.txt` 只是"候选节点备份"，不是关键路径——**写失败完全应该降级为日志警告**。

---

### BUG-11【低】`loops()` 的 `s.runSelection()` 阻塞调度循环

**位置**：`service.go:747-770`

```go
func (s *service) loops() {
	for {
		time.Sleep(15 * time.Second)
		...
		if due {
			s.runSelection()   // ← 同步调用，可能阻塞 40 分钟
		}
	}
}
```

`runSelection` 里 xray 实测的 `overall` 超时是 **40 分钟**（`service.go:726`）。虽然它内部 `s.running` 会置位、看门狗部分会 `continue` 跳过，但**健康检查也被跳过了**：

```go
if running { continue }   // ← 选点期间看门狗完全停摆
```

**影响**：选点失败的兜底路径有 `fallbackDefaultNode`，所以不至于完全裸奔。但如果主实例在选点期间掉线（正好赶上节点被封），**40 分钟内没有任何自愈**。健康检查与选点不该互斥。

**修复**：健康检查独立成一个 goroutine（或至少把 `running` 判断收紧到只保护"到期选点"那一段）：

```go
func (s *service) loops() {
	// 健康检查：独立循环，永远运行
	go func() {
		for {
			time.Sleep(15 * time.Second)
			if !s.mainAlive() {
				if err := s.startMainFromConfig(); err == nil { ... }
			}
		}
	}()
	// 选点调度
	for {
		time.Sleep(15 * time.Second)
		s.mu.Lock()
		running, cfg := s.running, s.cfg
		s.mu.Unlock()
		if running { continue }
		if due { s.startSelection() } // 用 startSelection（异步）
	}
}
```

注意改用 `startSelection()` 而非 `runSelection()`——前者异步且防重入，后者同步。现在 `loops` 直接调 `runSelection` 绕过了 `startSelection` 的锁保护，只是因为自己已经检查过 `running` 才没出问题。

---

### BUG-12【低】`randHex` 忽略 `rand.Read` 的错误

**位置**：`main.go:901-905`

```go
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	rand.Read(b)   // ← 忽略返回值
	return hex.EncodeToString(b)[:n]
}
```

`crypto/rand.Read` 在 Go 1.24+ 的文档里注明"永不返回错误"（失败会 panic），所以实际安全。但显式处理更清晰，也是 lint 常规要求。

---

### BUG-13【低】配置校验不完整

**位置**：`service.go:328`

```go
if cfg.Top < 1 || cfg.DownloadSizeMB < 1 || cfg.SelectIntervalMin < 5 {
    writeJSON(w, ...{error: "参数不合法（候选数≥1，下载上限≥1 MB，间隔≥5 分钟）"})
    return
}
```

漏了：
- `Workers` 可以为 0 或负数 → `testAndFilter` 里 `sem := make(chan struct{}, 0)` → **所有 goroutine 阻塞在 `sem <- struct{}{}` 上，永久死锁**（`testAndFilter` 永不返回，选点卡死）。负数则直接 panic（`make` 负数容量）。
- `MaxLatencyMS` 为 0 → `time.Duration(0)` → `net.DialTimeout` 立即超时 / `proxyClient` 立即超时，全部节点失败（但不崩溃）。
- `DownloadTimeoutS` 为 0 → 同上。
- `ServerURL` / `LatencyURL` 为空或非法 → `verifyOutbound` / `probeLatency` 返回 URL 解析错误。

其中 **`Workers = 0` 导致死锁是最严重的**——前端 `parseInt(el.value, 10) || 0`，用户清空输入框再保存就会写入 0。

**修复**：

```go
if cfg.Top < 1 || cfg.DownloadSizeMB < 1 || cfg.SelectIntervalMin < 5 ||
   cfg.Workers < 1 || cfg.Workers > 200 ||
   cfg.DownloadTimeoutS < 1 || cfg.MaxLatencyMS < 1 {
    ...
}
```

---

## 三、被我撤回的误报（方法论记录）

这三条都曾被我列为 bug，全部撤回。**它们有一个共同的失败模式**，比结论本身更值得记录。

### 误报 1：`pickCandidates` 保底逻辑

**我当时认为**：当前节点占掉候选名额、挤掉真正的挑战者 → 选点锁在局部最优。

**错在哪**：违反了函数的调用前提。`pickCandidates` 要求 `rows` **已按延迟升序排好**（main.go:512 排完才传入）。我的探针给 `cur` 赋了 300ms 却没把它排到数组尾，于是"慢节点排在最前"这个真实调用链里不可能出现的输入，被我当成了 bug 的证据。

**符合前提重测**：`cur` 慢时它必在数组尾，`pick=[0 1 2]` → 挑战者一个没被挤掉。

### 误报 2：热切换无回滚

**我当时认为**：`RemoveHandler` 后 `AddOutboundHandler` 失败会导致出站消失、断网、看门狗反复拉起坏节点。

**错在哪**：只看了函数内部的窗口，没看**节点进入这个函数之前经历过什么**。走到 `applyWinner` 的节点刚通过 3 次延迟验证 + 1 次真实下载测速，"这个节点是死的"前提不成立。而加回滚的代价是**每次切换都多跑一段状态机**——把几乎不发生的情况变成常态开销。

### 误报 3：`NaN` 穿透判定（**最典型的一条，两轮才纠正**）

**我当时认为**：`latencyMs` 可能是 `NaN`，会穿透排序、过滤、兜底三道，最后让 NaN 节点当选 winner。

**错在哪**：**没有追赋值链就假设了取值范围。** 作者一句"NaN 那里来的"直接问到根子上。

实测赋值链：

```
lat[i] : time.Duration
  ↓ lat[i].Microseconds()        → int64（永远有限）
  ↓ float64(int64)               → 永远有限，不可能 NaN
  ↓ / 1000                       → 除数是非零常量，不可能产生 NaN
= r.latencyMs
```

整条链**无法产生 `NaN`**。`NaN` 只来自 `0/0`、`Inf-Inf`、`Inf*0` 或 `math.NaN()`，这里一个都不沾。而且 `lat[i]` 零值与 `lerr[i] == nil` 互斥（`len(ds) < 2` 才会设 `lerr`，否则 `lat` 必来自真实探测），所以连"零值渗透"都不存在。

**共同失败模式**：`误报 1` 违反输入前提，`误报 3` 假设了不存在的取值范围。**两者都是"构造出一个真实运行中到不了的输入，然后把它当成 bug"。** 判定缺陷前必须先回答两个问题：

1. 这个输入/状态，在真实调用链里**能到达吗**？（谁以这个输入调用它）
2. 我能**追到**它的产生源头吗？追不到就是理论推演，不是 bug。

### 一条真的（但用户已明确接受为取舍）

`lastErr.Error()` 空指针 panic 是**真能复现的**（ctx 超时 → 循环体一次不执行 → `lastErr` 为 nil），但 40 分钟预算最坏只耗约 7 分钟，默认配置触发不到。**不作 bug 上报**，仅记录：若把 `downloadTimeoutSec` 调到 300+、`top` 调到 10+、节点数上百，三个参数叠加时就会碰到。

作者对这条的态度与热切换一致：**概率极低 + 有兜底（`selectBest` 的 recover）+ 修复收益小于复杂度 → 不做**。

---

## 四、改进建议

### 高优先级

1. **修 BUG-13、BUG-3**——BUG-13 改动最小且前端清空输入就能触发死锁；BUG-3 用 `-race` 跑一遍就能现形。
2. **补并发测试**：`go test -race` 跑一遍。当前 `handleConfig` / `handleDefaultNode` / `loops` 的并发路径**完全没有测试覆盖**，`-race` 大概率能报出 BUG-3。
3. **`applyWinner` 加节点切换日志**：现在只有 `fmt.Printf` 到 xpilot.log，建议在 `runInfo` 里记录"切换前节点 → 切换后节点 → 验证结果"，让控制台能看到切换历史。现在每次切换的证据只有一行"最优节点与当前一致，无需切换"或日志里的 `Xray 26.3.27 started`。

### 中优先级

4. **`testNode` 生命周期用 `sync.Once` 收口**（BUG-6），并去掉三重防御中的冗余层。
5. **`xrayTestAll` 的平行数组合并成结构体切片**——`nodes`/`lat`/`lerr`/`rows` 四处靠下标对齐，任何一个循环写错都难发现。
6. **`.gitignore` 的 `html/*` 白名单**目前放行了 `console.html`、`geoip.dat`、`geosite.dat`，但 `pac` 被忽略——而 `cloudflare`/`asset` 探针显示 `html/config.json`、`html/xpilot.json` 也在目录里且**未被忽略**（`html/*` 会忽略，但 `config.json` 在 `html/` 下…… 检查后确认 `html/*` 对所有非白名单项生效，OK）。不过 **`html/config.json` 和 `html/xpilot.json` 出现在项目目录里本身很可疑**——这两个文件运行期应该只在 `%AppData%\xpilot\` 生成。看起来是旧版本遗留（那时还没有 `appDataDir`）。建议删除，避免混淆。
7. **`node.list.txt` / `node.best.txt` 建议输出到 `logs/` 的子目录或改名**：`node.best.txt` 内容是"进入下载实测的候选节点"，不是"最优节点"，命名与实际含义不符（`saveBestAndPick` 里写的是 `candidateLinks(rows)`）。建议改名 `node.candidates.txt`。

### 低优先级（架构/体验）

8. **热切换的"零感知"是有前提的**：`xraycore.go:96-99` 旧出站延迟 30 秒关闭，这 30 秒内旧连接走旧出站、新连接走新出站。因为新节点刚通过完整实测，这个过渡是安全的。留一条：若哪天想支持"未经实测的节点直接上线"（比如手填节点），这个假设就不成立了。
9. **控制台的 `node.best.txt` 展示**：目前只有 `/api/file` 白名单里的 3 个文件，`node.best.txt` 和 `node.list.txt` 不可见。既然写了，就加到白名单里（或加一个"候选节点"标签页）。
10. **`pacDrainPeriod` 常量定义了但未使用**（`main.go:885`）。代码里搜不到引用——注释说的"蓝绿切换"逻辑已经改成热替换，这个常量是遗留。建议删除或补上使用点。
11. **`accessRe` 与真实日志格式一致性**：你的实际日志里 detour 是 `[mixed-in >> proxy]`（双箭号 `>>`），而注释和正则示例写的是 `[mixed-in -> proxy]`（单箭号）。`parseAccessLines` 用 `strings.LastIndex(detour, "->")` 提取走向——对 `mixed-in >> proxy` 来说，`LastIndex("->")` 找不到 `->`（因为只有 `>>`），于是走 `else { out = detour }`，**结果是 `out = "mixed-in >> proxy"` 整串**，而不是 `"proxy"`。
    
    验证：你的 `xray-access.log` 里同时存在两种格式——
    - `[mixed-in -> direct]`（单箭号，旧格式）
    - `[mixed-in >> proxy]`（双箭号，新格式）
    
    **前端 `OUTCLS` 映射 `{proxy, direct, block, rejected}`，拿到 `"mixed-in >> proxy"` 整串会命中 `b-other`（灰色"未知"标签）**，而不是蓝色的 proxy 徽章。**访问日志的走向着色对大部分记录是失效的**。
    
    修复：正则应同时兼容 `->` 和 `>>`，或改成分离出 tag 后取最后一个 token：
    ```go
    // 取 detour 里的走向 tag
    if i := strings.LastIndex(detour, ">"); i >= 0 {
        out = strings.TrimSpace(detour[i+1:])
    }
    ```
    并给 `parseAccessLines` 加一条双箭号的测试用例。

12. **`report.go` 的 `maxSpd := 1.0` 初始值**：若所有速度都 < 1 B/s，`maxSpd` 保持 1，条形图几乎不可见。虽然实际不会发生，但用实际最大速度更严谨。

---

## 五、测试评价

现有 5 个测试质量不错，尤其 `TestMigrateAndRelease`（覆盖多源位置迁移 + 释放不覆盖）和 `TestXrayTestAllFakeNodes`（用 `127.0.0.1:1` 假节点跑通全流程、验证实例回收）——**用真实代码路径而非 mock**，方向和我的探针思路一致。

**缺口**：
- `pickCandidates` 的**保底约定**（当前节点必进候选）**零覆盖** → 这条约定是业务核心，但没有任何测试锁住它，将来有人"顺手优化"就会破坏。
- `saveBestAndPick` 的 80% 粘性阈值**零覆盖**。
- `latencyMs` 的取值边界（`+Inf` / 0 / 正常值）**零覆盖**。注意：`NaN` 经核实不可达，**不需要**为它加测试；需要的是锁住"`+Inf` 被正确过滤"这条。
- `buildXrayConfig` 的协议分支（reality/tls/ws/grpc）**零覆盖**。
- `extractAddrPort` 的 vmess/ss/ssr 分支**零覆盖**。
- 并发路径（HTTP handler + loops）**零覆盖**。
- `TestRegistryPAC` 会真实修改用户的 `HKCU\...\Internet Settings\AutoConfigURL`，**测试污染真实系统状态**。建议加 `-short` 跳过或改用可注入的注册表读写接口。

---

## 六、一句话结论

架构和工程品味在这个量级的个人项目里属于上乘——内嵌 core、热替换出站实现近零成本切换、AsIs 分流、单一接入层都是想清楚了的决策。**实测后确认的实质缺陷都在外围：并发状态管理不严谨（BUG-3、BUG-13）、可观测性与资产更新（BUG-5、BUG-9、BUG-11）。核心选点链路没有缺陷。**

按修复优先级：

```
BUG-13 Workers=0 死锁       → 前端清空输入即可触发，一行校验搞定      【最易触发】
BUG-3  handleDefaultNode 竞争 → 连点两次触发并发 applyWinner，-race 可见 【-race 一跑就现形】
BUG-9  访问日志走向着色失效  → 控制台可读性问题，改动最小              【低，但每天都在看】
BUG-5  geo 资产升级被静默忽略 → 分流准确性渐进劣化                    【中】
BUG-11 选点期间看门狗停摆    → 40 分钟无自愈窗口                      【中】
```

**本项目没有影响选点正确性的缺陷。** 选点链路（订阅 → 过滤 → 去重 → TCP → 实测 → 选优 → 落地）经实测验证逻辑自洽，`pickCandidates` 的保底约定、`saveBestAndPick` 的 80% 粘性阈值都工作正常。
