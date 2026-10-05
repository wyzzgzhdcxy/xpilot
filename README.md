# xpilot

Windows 上常驻运行的**代理选点管家**：从机场订阅拉节点、按真实网络质量挑出最快的一个，
以**热替换**方式无感切换，并对外提供稳定的本地代理端口与 PAC 自动分流。

单文件 exe，内嵌 xray-core **库**（不依赖外部 `xray.exe`），无控制台窗口，开机即静默运行。

---

## 目录

- [它解决什么问题](#它解决什么问题)
- [核心特性](#核心特性)
- [快速开始](#快速开始)
- [工作原理](#工作原理)
  - [选点流水线](#选点流水线)
  - [双层分流架构](#双层分流架构)
  - [PAC 文件的结构与判定口径](#pac-文件的结构与判定口径)
  - [热替换（蓝绿切换）](#热替换蓝绿切换)
- [架构与模块](#架构与模块)
- [运行期文件](#运行期文件)
- [配置项](#配置项)
- [HTTP 接口](#http-接口)
- [控制台使用说明](#控制台使用说明)
- [构建与打包](#构建与打包)
- [测试](#测试)
- [关键设计决策](#关键设计决策)
- [已知限制与注意事项](#已知限制与注意事项)

---

## 它解决什么问题

机场订阅通常给你几十上百个节点，但：

- **节点质量差异极大**——延迟低的可能带宽很差，TXT 里看着都差不多。
- **锁的节点会失效**——昨晚还快的节点，今天可能已经挂了，但客户端不会自动换。
- **手动换节点要重启客户端**——正在下载的东西会断。次数多了非常烦。
- **全局代理会拖慢国内访问**——访问百度也要绕一圈国外，纯属浪费。

xpilot 的做法是：**定期把所有节点都真跑一遍，按实测速度挑冠军，然后把冠军原地换到正在监听的端口上**——
客户端一直连着 `127.0.0.1:10808`，中间换节点它完全不知道。同时用 PAC + xray 路由做两级分流，国内流量直连不绕路。

---

## 核心特性

| 特性 | 说明 |
|---|---|
| **实测选点** | 不是看延迟排名，而是真起 xray 实例下载测速，按实际吞吐量选冠军 |
| **零中断切换** | 热替换 proxy 出站，监听端口与实例全程不动，旧连接延迟 30 秒排空 |
| **两级分流** | PAC 决定「要不要走代理」，xray 路由决定「走节点还是直连」，国内流量不绕路 |
| **单文件部署** | 一个 exe，内嵌 xray-core 与 geoip/geosite 资产，可在任意位置离线运行 |
| **Web 控制台** | 参数设置、路由规则查看、访问日志、PAC 编辑，全部在浏览器里 |
| **实时进度推送** | SSE 推送选点阶段，控制台不用轮询，选点过程看得见 |
| **掉线自愈** | 看门狗每 15 秒检查主实例，挂了就用现有配置原地拉起 |
| **失败兜底** | 所有节点都挂时，回退到预设的默认节点，不至于彻底断网 |
| **粘性选点** | 新最优节点若比当前节点快不到 20%，保持不动，避免无谓切换 |

---

## 快速开始

### 1. 编译

```powershell
# 方式一：直接构建到当前目录
go build -trimpath -ldflags "-s -w -H=windowsgui" -o xpilot.exe .

# 方式二：用打包脚本（输出到 E:\application\我的工具箱）
pwsh -File build.ps1
```

> `-H=windowsgui` 是关键：让程序以无控制台窗口方式运行。不加的话启动会弹一个黑框。

### 2. 运行

双击 `xpilot.exe`，或加入开机自启：

```powershell
# 注册表启动项（推荐）
reg add "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v xpilot /t REG_SZ /d "E:\application\我的工具箱\xpilot.exe" /f

# 或者用计划任务（需要管理员权限）
schtasks /create /tn xpilot /tr "E:\application\我的工具箱\xpilot.exe" /sc onlogon /rl highest
```

启动后程序会：

1. 在 `%AppData%\xpilot` 建好目录、释放内嵌资源
2. 关闭系统代理的「自动检测」、把 PAC 指向 `http://127.0.0.1:6001/pac`
3. 拉起 Web 控制台（`http://127.0.0.1:6001/`）与主实例
4. 进入常驻循环：健康检查 + 到期选点

### 3. 配置订阅

打开 <http://127.0.0.1:6001/> → **参数设置** → 填入订阅地址（每行一个）→ 保存 → 回**运行状态**点「立即选点」。

首次运行没配订阅时会走兜底逻辑：用 `defaultNodeLink` 起主实例，保证有代理可用。

### 4. 让浏览器走代理

程序启动时已自动把系统代理设为 PAC 模式，Chrome / Edge / IE 等会自动跟随系统设置。
Firefox 默认不跟随系统 PAC，需要手动设置：
`设置 → 网络设置 → 自动代理配置 URL` 填 `http://127.0.0.1:6001/pac`。

---

## 工作原理

### 选点流水线

一次完整选点（`selectBest`）分七步：

```
订阅地址（可多个）
    │
    ▼
① 拉取订阅 ──────── 带 v2rayN UA，保证机场返回 base64 而非 clash YAML
    │              多行 base64 / 明文链接列表都能解
    ▼
② 协议过滤 ──────── 按 proto 匹配链接 scheme（默认只留 vless）
    │
    ▼
③ 按 地址:端口 去重 ── 不同机场可能给同一台服务器的不同端口，去重省时间
    │
    ▼
④ TCP 连通测试 ──── 等价 telnet，1 秒超时，并发数由 workers 控制
    │              连不上的直接淘汰（不做无谓的 xray 实例开销）
    ▼
⑤ 逐节点起 xray 实测 ─ 每节点一个内存实例 + 随机本地端口
    │              先并发跑 3 次协议验证（≥2 次通过取中位数，否则淘汰）
    │              延迟按中位数算，抗抖动
    ▼
⑥ 候选节点下载测速 ── 延迟前 top 名（每 IP 只占一个名额）串行下载
    │              串行避免共享本机带宽互相干扰
    ▼
⑦ 选冠军 + 落地 ──── 下载速度最快者胜；当前节点若达最优 80% 则保持不动
                   热替换主实例出站 → 回写 config.json → 写 HTML 报告
```

几个细节：

- **协议验证跑 3 次**：`probeLatency` 探测时必须真的能连通才算「协议可用」，避免只 TCP 通但协议配置错的节点混进候选。3 次里至少 2 次通过才保留，取中位数作为延迟（2 次通过时取较差那次）。这样能过滤掉时好时坏的抖动节点。
- **每 IP 只占一个名额**：机场经常在一台服务器上开十几个端口，如果按纯延迟排序，候选名单会被同一台机器包揽，测速就失去意义了。所以 `pickCandidates` 按 IP 去重。
- **当前节点保底入围**：正在使用的节点强制进入候选，让它和新挑战者同场竞技，避免「没测过就换掉」。
- **实测与生产共用配置生成逻辑**：`buildXrayConfig` 一个函数两处用。区别只在 `withRouting` 参数——测速实例传 `false` 跳过分流规则（省去 geoip/geosite 加载开销，且保证测速流量全部走节点）。

### 双层分流架构

```
浏览器请求 https://www.google.com
  │
  ├─ ① PAC 判定（在浏览器进程内执行）
  │     ├─ DIRECT ──────────────→ 浏览器直连（完全不经过 xpilot）
  │     └─ PROXY 127.0.0.1:10808 → 交给本地 xray
  │                                  │
  │                                  ▼
  └─────────────────── ② xray 路由判定（在 xpilot 进程内执行）
        ├─ 内网 IP / 国内域名 / 国内 IP ──→ direct（本机直连出去）
        └─ 其余 ────────────────────────→ proxy（当前选中的节点）
```

**两层各管一段，任何一层判为直连，流量都不会走节点。**

PAC 内容来自 `%AppData%\xpilot\html\pac`，是**用户自己的文件**，xpilot 不生成它（只在缺失时释放一份起始模板），
只负责通过 `http://127.0.0.1:6001/pac` 提供给系统。想加自定义分流规则直接改这个文件。

xray 路由则由 `buildXrayConfig` 写死，规则固定三条 + 一条兜底：

| 顺序 | 匹配条件 | 走向 | 说明 |
|---|---|---|---|
| 1 | 目标 IP ∈ `geoip:private` | direct | 127.0.0.1、192.168.x.x、10.x.x.x 等本机/内网地址 |
| 2 | 目标域名 ∈ `geosite:cn` | direct | 国内域名清单（baidu.com、qq.com 等） |
| 3 | 目标 IP ∈ `geoip:cn` | direct | 国内 IP 段，主要命中纯 IP 访问的目标 |
| 4 | 其余全部流量 | proxy | 未命中上述规则的走第一个出站，即当前节点 |

注意规则 2 与 3 用的是**两种不同的数据**：`geosite` 是**域名清单**（按域名匹配），
`geoip` 才是**IP 归属地库**（按 IP 段匹配，带国家码）。名字像，作用完全不同。

入站是 `mixed` 协议（socks5 与 http 同端口，允许 UDP），并开启流量嗅探（http/tls/quic）——
从 TLS SNI、HTTP Host 还原真实目标域名，否则域名规则匹配不到。

### PAC 文件的结构与判定口径

`html/pac` 是**域名白名单**模式，一句话就能说清：

> host 命中列表里的任一条 → 走代理；否则**直连**。

```javascript
var proxy = 'PROXY 127.0.0.1:10808;DIRECT;';
var proxyList = [ /* 6788 条域名 */ ];

function FindProxyForURL(url, host) {
    host = host.toLowerCase();
    return matchAny(host, proxyList) ? proxy : "DIRECT";
}

function matchAny(host, list) {
    for (var i = 0; i < list.length; i++) {
        var rule = list[i];
        if (host === rule || host.endsWith("." + rule)) return true;
    }
    return false;
}
```

**匹配规则**：`host === rule`（精确）或 `host.endsWith("." + rule)`（子域名）。
后者**必须带点** —— 否则 `notgoogle.com` 会被 `google.com` 误命中，是典型的后缀陷阱。
另外**不要用 `Array.includes()` 做匹配**：它只比精确相等，会漏掉 `api.github.com`
这类子域名，看起来「部分网站莫名直连」，很难排查。

**为什么不写成 `includes` 一行**：域名列表来自 gfwlist 转换工具，原始产出是
`[[], [域名…]]` 两层结构 + 奇偶交替（`rules[i%2]` 决定 DIRECT/PROXY）。
那是通用模板，为支持「任意多条规则」而设计；本项目只用一条白名单规则，
多余的层级已展开成平铺数组。转换与验证脚本在 `tools/`：

| 脚本 | 作用 |
|---|---|
| `tools/pac_rewrite.js` | 把 `html/pac.src`（转换工具产出的原始版）展开成简洁版 `html/pac` |
| `tools/pac_equiv.js` | **等价性验证**：把两版判定函数对 3.3 万个用例逐条比对 |
| `tools/bench_pac_frame.js` | **渲染性能实测**：通过 Edge CDP 量 PAC 域名清单的渲染开销（需先按脚本头注释启动 Edge 调试端口） |

更新域名列表的流程：

```powershell
# 1. 用 gfwlist 转换工具生成原始版，存为 html/pac.src
# 2. 展开成简洁版
node tools/pac_rewrite.js
# 3. 验证行为等价（退出码 0 才算通过）
node tools/pac_equiv.js
```

**关于 `toLowerCase()`**：新版主动把 host 转小写，原版不做（依赖 PAC 规范
「host 由浏览器转小写」的保证）。规范内两者行为一致；一旦输入非小写，
原版会**静默直连**而新版正确判为代理 —— 属有意的健壮性改进。
`pac_equiv.js` 会把这类差异单独归类计数，不会与真正的逻辑差异混淆。

**关于渲染条数上限（200 条）**：域名清单全量 6788 条约 89KB、整页高 38000px，
一次性塞进 DOM 会让浏览器做 600+ 行的逐字符断行与布局。实测（赋值 + 强制同步布局）：

| 渲染条数 | 平均耗时 | 内容体积 | 页面高度 |
|---|---|---|---|
| 全量 6788 条 | 4.7 ~ 9.2 ms（最差可达 18.4 ms） | 89 KB | 38000 px |
| 1000 条 | 0.75 ms | 12.9 KB | 5514 px |
| 200 条 | 0.20 ms | 2.4 KB | 525 px |

全量渲染的最差耗时已超过 60fps 的 16.7ms 帧预算，输入时会掉帧；而**纯 JS 过滤只要
0.2ms** —— 所以瓶颈在 DOM 渲染，不在搜索算法。超出部分提示「继续输入缩小范围」。
完整列表始终可从 `%AppData%\xpilot\html\pac` 或 `/pac` 端点获取，页面只是查看器。

**关于卡片头部的 flex 排版**：`.fhead` 是 flex 容器，放了标题、路径、计数、搜索框四项。
两条容易踩的规则：

- 标题与计数必须 `flex:0 0 auto` + `white-space:nowrap`。否则空间不足时它们会被压到
  逐字换行 —— 中文标题尤其明显（「PAC 域名列表」会竖成一列）。空间让 `.fpath` 承担。
- 搜索框必须写成 `.fhead .pacsearch` 并放在通用规则 `input[type=text]{width:100%}`（第 114 行）
  **之后**。同优先级下后写的胜，否则搜索框会被拉满整行、并把路径挤成 0 宽。

**关于删除的 `endsWith` polyfill**：原版文件尾部有一大段
`if (!String.prototype.endsWith) { … }`（照抄自 MDN 官方 Polyfill 段落），
新版已删除。判断依据：

- 该 polyfill 是 gfwlist 转换工具在 2015 年前后为兼容老浏览器塞进模板的，
  属于通用兜底，与 Firefox 无关（注释里的 MDN 链接容易让人误以为是修
  Firefox 的 bug，实际不是）。
- `endsWith` 自 ES6（2015-09）进入基线，Firefox 17 起原生支持。
  如今在用的浏览器全都自带，这段代码是**纯空转**。
- PAC 跑在受限的 JS 沙箱（Firefox 为 SpiderMonkey）里，脚本作用域受限，
  DOM / `window` / 扩展 API 均不可用。在沙箱里改内建 `String.prototype`
  风险大于收益：若 `prototype` 属性只读或引擎禁用该操作，会**抛异常并
  中断整个 PAC 文件**，比不加更糟。
- 因此若未来真需要兼容极老引擎，**正确做法是不依赖 `endsWith`**
  （如 `host.slice(-(rule.length + 1)) === "." + rule`），而不是加回 polyfill。

新版实际语法依赖（已剥离注释实测）：仅 `var`、普通 `for` 循环、
`.endsWith()`、`.toLowerCase()` —— 未使用 `let`/`const`、箭头函数、
模板字符串、解构、`Array.includes()` 等任何其它 ES6+ 特性。

### 热替换（蓝绿切换）

这是「切换节点不断网」的核心。常规做法是杀掉旧实例、起新实例，中间有几秒断网。
xpilot 的做法是**只换出站，不动实例**：

```go
// xraycore.go: swapOutbound
obManager := x.inst.GetFeature(outbound.ManagerType()).(outbound.Manager)
oldHandler := obManager.GetHandler("proxy")

// 1. 把旧的 proxy 出站从路由表摘掉（仅摘表，不断开已有连接）
obManager.RemoveHandler(context.Background(), "proxy")

// 2. 挂上新的 proxy 出站
core.AddOutboundHandler(x.inst, newHandlerConfig)

// 3. 旧出站延迟 30 秒关闭，让在途连接自然排空
go func() {
    time.Sleep(30 * time.Second)
    oldHandler.Close()
}()
```

因为监听端口 `10808` 和入站配置全程没变，浏览器侧的 TCP 连接不受影响；
已在传输中的连接由旧出站继续服务到自然结束，新连接立刻走新节点。

`applyWinner` 里的落地分支：

| 主实例状态 | 动作 |
|---|---|
| 未运行 | 直接用该节点启动主实例 |
| 运行中且节点未变 | 什么都不做（省掉不必要的切换） |
| 运行中且节点变 | 热替换出站；失败则回退为「重启实例」 |
| 热替换成功 | `verifyOutbound` 做一次出网检查，失败只在日志里告警（不回滚） |

落地后会把当前节点的完整配置回写 `config/xpilot.json`（实为 `config/config.json`），
供下次开机自愈和粘性判断读取。

---

## 架构与模块

```
xpilot/
├── main.go         1002 行 核心逻辑：订阅解析、TCP 测试、xray 实测、选优、配置生成、注册表
├── service.go      1029 行 常驻服务：HTTP 路由 + 调度循环 + 选点流水线编排 + 热替换落地
├── report.go        392 行 运行报告：单文件 HTML 生成（内联样式与条形图，离线可看）
├── assets.go        167 行 内嵌资源释放与旧版文件迁移
├── xraycore.go      111 行 xray-core 库的统一接入层（直接依赖 xray-core 的文件之一）
├── main_test.go     564 行 单元测试
├── console.html     681 行 Web 控制台页面（内嵌进 exe）
├── build.ps1         78 行 打包脚本（停进程 → 构建 → 替换 → 起进程）
├── tools/
│   ├── pac_rewrite.js      把原始 gfwlist 版 pac 展开成简洁版
│   ├── pac_equiv.js        两版 pac 的等价性验证（3.3 万用例）
│   ├── bench_pac_frame.js  PAC 域名清单的渲染性能实测（Edge CDP）
│   └── verify_console.js   控制台页面关键行为的浏览器实测（Edge CDP）
└── html/
    ├── console.html      控制台页面源文件
    ├── pac               简洁版 PAC 规则（域名白名单，用户文件）
    ├── pac.src           原始 gfwlist 转换产物（pac_rewrite.js 的输入）
    ├── geoip.dat         19.8 MB  xray 路由用的 IP 归属地库
    └── geosite.dat       10.5 MB  xray 路由用的域名分类库
```

### 各文件职责

**`main.go`** — 不含状态的核心函数集合，全部是纯逻辑或独立工具：

- `fetchBody` / `decodeSubscription` — 拉订阅并还原成链接列表
- `extractAddrPort` — 从 vmess/ss/ssr/vless/通用链接里抠出 `地址:端口`
- `testAndFilter` — 并发 TCP 连通测试（信号量控制并发）
- `xrayTestAll` — 实测主流程：起实例 → 延迟验证 → 下载测速
- `pickCandidates` / `saveBestAndPick` — 候选筛选与冠军评选（含 80% 粘性逻辑）
- `buildXrayConfig` — 节点链接 → xray JSON 配置（唯一权威来源）
- `killPortListeners` / `portListening` / `hideWin` — Windows 进程与端口工具
- `setRegistryPAC` / `readPacURL` — 系统代理注册表操作

**`service.go`** — 所有带状态的东西都在这里：

- `service` 结构体持有配置、运行状态、xray 实例、SSE 广播中心
- `listen` 注册 HTTP 路由 + `loopbackOnly` 中间件
- `selectBest` 串起完整选点流水线
- `applyWinner` / `swapMainOutbound` / `fallbackDefaultNode` 负责落地与兜底
- `loops` 常驻循环：15 秒一次健康检查 + 到期触发选点

**`xraycore.go`** — **单一接入层**。所有对 `xray-core` 包的直接依赖（`core` / `serial` / `distro` / `outbound`）
都收敛在这里，其余代码只使用这里暴露的 `xrayInstance` 类型与三个函数。
好处是升级 xray-core 版本时，只需要检查这一个文件是否需要适配。

**`assets.go`** — exe 单文件的关键：

- `releaseEmbedded` 把内嵌资源释放到数据目录。`console.html` 每次启动都覆盖（保证页面与程序版本一致），
  `pac` / `geoip.dat` / `geosite.dat` 只在缺失时写出（不覆盖用户修改）
- `migrateLegacyFiles` 把旧版本散落在 exe 目录里的运行文件迁到数据目录，
  兼容根目录 / `config/` / `html/` / `logs/` 四个历史位置

**`report.go`** — 每次选点生成一份 `xpilot-report.html`，含统计卡片、订阅源状态、
延迟对比条形图、下载速度对比条形图、全节点明细表。纯内联 CSS/JS，无外部依赖，离线可看。

---

## 运行期文件

**exe 可以在任意位置运行**，所有运行期数据集中在用户数据目录：

```
%AppData%\xpilot\
├── config\
│   ├── xpilot.json          用户参数（订阅地址、阈值等），控制台可编辑
│   └── config.json          当前节点的完整 xray 配置，每次落地时生成，供开机自愈使用
├── html\
│   ├── console.html         控制台页面（每次启动由 exe 内嵌版本覆盖）
│   ├── pac                  PAC 规则文件（用户自行维护，域名白名单模式）
│   ├── geoip.dat            IP 归属地库（缺失时自动释放，xray 路由需要）
│   ├── geosite.dat          域名分类库（缺失时自动释放，xray 路由需要）
│   └── xpilot-report.html   最近一次选点的运行报告
└── logs\
    ├── xpilot.log           主程序日志（程序无控制台，所有输出都在这里）
    ├── node.list.txt        TCP 测试通过的全部节点
    ├── node.best.txt        进入下载实测的候选节点
    ├── xpilot-access.log    访问日志（文本，界面取末尾 200 行）
    ├── xpilot-select.log    选点历史（每行一条 JSON，完整留存、重启不清空）
    └── xpilot-error.log     错误日志（warning 及以上，由 xpilot 接管写入）
```

> **访问日志是排查分流问题的最佳工具。** 想确认某个域名走了直连还是代理，
> 看控制台的「访问日志」标签页即可（每 3 秒自动刷新，支持按域名 / IP / 走向过滤）。
> 也可以直接看文件：
>
> ```powershell
> Get-Content "$env:AppData\xpilot\logs\xpilot-access.log" -Tail 20
> ```
>
> 日志文件名以 `xpilot-` 开头是因为**写入方是 xpilot 自己**（见下节）。
> 从旧版升级时，`xray-access.log` / `xray-error.log` 会在首次启动时**就地改名**
> 为 `xpilot-*.log`：内容保留、不合并；若新名已存在则放弃改名，绝不覆盖。

### 选点历史文件

界面上最多看 100 条，**完整历史在 `logs/xpilot-select.log`**，每行一条 JSON：

```powershell
# 看最近 5 次选点的 winner 与耗时
Get-Content "$env:AppData\xpilot\logs\xpilot-select.log" -Tail 5 |
  ConvertFrom-Json | Select-Object time, durationSec, ok, winner
```

字段与 `GET /api/config` 的 `history[]` 完全一致（`time` / `ok` / `winner` /
`durationSec` / `metrics` / `error`）。**这是追加文件，只增不裁剪**，想清空就直接删。

### 日志为什么由 xpilot 接管

xray 的日志 handler 是**进程级全局单例**，而 `app/log` 在**每个实例创建时都会
`RegisterHandler` 把全局 handler 抢走**。更麻烦的是实例 `Close()` 会把
`active` 置 false —— 于是那个 handler **留在全局、但吞掉所有日志**。

xpilot 的选点流程每次都要建销一批实测实例，所以如果不处理，
**日志会在选点那一刻永久断掉**，而代理完全正常、毫无异样，极难察觉。

修法：xpilot 自己实现 `log.Handler`（`xraylog.go`）且**永不关闭**，
在实例创建 / 销毁 / 出站热替换三处调用 `reclaimXrayLogs()` 把接管权夺回来；
同时 xray 侧 access / error 都配成 `"none"`，避免两处写入打架导致行重复。
`TestLogSurvivesInstanceChurn` 是这个行为的回归测试 —— 把 `reclaimXrayLogs()`
置空它就会失败，说明它盯的是真因而非巧合。

### 日志行的两处归一化（看着像 bug 的正常现象）

原始日志行形如：

```
2026/09/20 14:45:29.187247 from 127.0.0.1:13475 accepted //github.githubassets.com:443 [mixed-in >> proxy]
```

解析后界面上显示为 `github.githubassets.com:443` + `proxy`。两处归一化都必要：

- **目标里的 `//` 与 `tcp:`** —— xray 目标字段是 `网络类型:地址:端口`。**开启嗅探后**
  真实域名从 TLS SNI / HTTP Host 还原出来，此前的网络类型位置留空，
  于是写成 `//host:port`。同一份日志里三种形态混着出现：
  `tcp:github.com:443`、`//collector.github.com:443`、`127.0.0.1:7124`。
  而页面只关心「访问了哪个地址」，网络类型从端口就能看出 ——
  所以前缀一律剥掉，整列统一成 `host:port`（`stripNetType`）。
  来源列同理：几乎每行都带 `tcp:`，冗长且无排查价值，还会把列挤爆导致排版重叠。
- **走向里的 `>>`** —— 方括号里是完整链路 `入站 >> 出站`，部分版本写作 `->`。
  页面只关心最终命中的出站，所以取最后一个分隔符之后的部分（`cleanDetour`）。
  早期只识别 `->`，遇到 `>>` 就把整串透传，把右侧徽标撑成两行。

`TestParseAccessLinesRealFormat` 用本机真实日志形态钉住这三条。

`geoip.dat` / `geosite.dat` 缺失时主实例会起不来（路由规则需要它们），
所以启动时会检查并在日志里大声告警。

---

## 配置项

配置文件：`%AppData%\xpilot\config\xpilot.json`

**数字字段受两级校验**：前端（即时提示 + HTML5 原生 `min`/`max`/`required`）
与后端（`validateConfig`，防手改配置文件）。两处范围定义必须同步——
后端常量在 `service.go` 的 `cfgXxxMin/Max`，前端在 `console.html` 的 `FIELDS`。

| 键 | 含义 | 默认值 | 允许范围 | 说明 |
|---|---|---|---|---|
| `proto` | 协议过滤 | `"vless"` | 任意 scheme 名，空为不过滤 | 只保留该协议的节点链接 |
| `subscribeURLs` | 订阅地址 | `[]` | — | 每行一个，支持多订阅 |
| `defaultNodeLink` | 默认节点链接 | `""` | — | 所有节点失败时的兜底节点 |
| `top` | 下载实测候选数 | `5` | 1 ~ 50 | 延迟排名前 N 个进入下载测速 |
| `downloadSizeMB` | 单节点下载上限 | `10` | 1 ~ 1024 | 测试文件下载多少 MB 后停止 |
| `downloadTimeoutSec` | 单节点下载超时 | `60` | 5 ~ 1800 | 单位秒 |
| `maxLatencyMs` | 延迟验证超时 | `1000` | 100 ~ 60000 | 单次探测超时，超时算失败 |
| `selectIntervalMin` | 自动选点间隔 | `60` | 5 ~ 1440 | 单位分钟 |
| `serverURL` | 下载测速地址 | GitHub 的 FFmpeg 构建包 | — | 建议用大文件，能真实体现带宽 |
| `latencyURL` | 延迟探测地址 | `https://github.com/robots.txt` | — | **决定按哪条线路排名**，见下方说明 |
| `workers` | TCP 测试并发数 | `10` | 1 ~ 200 | ⚠️ 不允许为 0 |
| `tcpTest` | 启用 TCP 连通测试 | `true` | — | 关闭后所有节点直接进入 xray 实测 |

### 关于 `workers` 的硬约束

`workers` **绝不能为 0**。`testAndFilter` 内部用 `make(chan struct{}, workers)` 做信号量，
容量为 0 的无缓冲 channel 会让写入操作永久阻塞，整个选点流程死锁。
负数则会让 `make` 直接 panic。后端 `validateConfig` 会在保存时拦下这两种情况。

### 关于 `latencyURL` 的选择

延迟探测的目标**必须是国内无法直连的站点**。默认用 `github.com` 而不是 `gstatic.com`，原因是：

`gstatic.com` 在国内有 Google 的边缘节点（`203.208.x.x` 段），直连也能返回 204，
而 `203.208.x.x` 在 geoip 库里**属于中国 IP**——会被路由规则判成直连，绕过节点，
造成节点「假通过」。用 `github.com` 则不存在这个问题，而且延迟排名直接对准最关心的 GitHub 体验。

### 端口（写死，不可配置）

| 端口 | 用途 |
|---|---|
| `10808` | 主实例的 mixed 入站（socks5 + http 同端口） |
| `6001` | Web 控制台 + PAC 服务 |

**这两个端口故意不做成可配置项**：PAC 文件和系统注册表里的地址都与之绑定，
开放修改只会让用户改出一个「代理指向空端口」的状态。同理，`6001` 被占用时
程序直接 `os.Exit(1)` 退出——这同时兼作**单实例锁**（第二个实例必然撞端口）。

---

## HTTP 接口

服务只监听 `127.0.0.1:6001`，另有一道 `loopbackOnly` 中间件：
非回环来源的请求一律返回 403。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/` | 控制台页面。每次请求都从磁盘读 `html/console.html`，改完刷新即生效 |
| GET | `/pac` | 返回 PAC 规则文件，`Content-Type: application/x-ns-proxy-autoconfig` |
| GET | `/api/config` | 参数与状态快照：`cfg` / `defaults` / `running` / `stage` / `lastRun` / `history` / `pacURL` / `nextRun` / `currentAddr` |
| POST | `/api/config` | 保存参数。请求体是应用了部分更新的配置对象，保存前过 `validateConfig` |
| GET | `/api/events` | **SSE 长连接**。建立后先推一次当前阶段，之后实时下发 `{type,text}` 事件，15 秒心跳 |
| POST | `/api/run` | 立即选点。已在运行中返回 `{"ok":false,"error":"选点正在进行中"}` |
| POST | `/api/default-node` | 直接应用 `defaultNodeLink`，跳过选点（手动应急用） |
| POST | `/api/pac-refresh` | 重写注册表 PAC（带新随机串），让手改过的 PAC 立即生效 |
| POST | `/api/open-regedit` | 启动注册表编辑器并定位到系统代理所在项 |
| GET | `/api/access-log` | 访问日志文件末尾的解析结果，**最多 200 条**、新记录在前 |
| GET | `/api/file?name=` | 读取配置文件内容。`name` 只接受白名单：`pac` / `xray` / `xpilot` |
| GET | `/html/*` | 静态文件服务，指向数据目录的 `html/`（用于打开运行报告） |

### SSE 事件格式

`/api/events` 推送两种事件：

```json
{"type":"stage","text":"拉取订阅中"}
{"type":"done","text":"选点完成：<节点名>（<地址>，<延迟>ms，<速度>）"}
```

前端逻辑：`stage` 只更新阶段文字与按钮状态；`done` 触发一次完整的 `GET /api/config` 快照刷新。
这样既保证实时性（阶段变化立刻可见），又避免高频轮询。

### 按钮的「乐观置灰」

`/api/run` 与 `/api/default-node` 这两个会启动后台任务的接口，
后端都在**写 HTTP 响应之前**就同步置位 `running = true`（微秒级操作），
所以前端点击后直接本地置灰、不等网络往返——更准，也堵住了「没变灰 → 以为没点上 → 连点」的窗口。

只有请求被拒（如返回 `ok:false` 的「选点正在进行中」）时才复原按钮并弹提示。
`act()` 里不再有 `setTimeout(refresh, 500)` 那种硬等——请求返回即拉快照对齐。

### 选点历史

「运行状态」页的**选点详情**是一个只追加的历史列表：

- **首行固定是「待执行时间」**，由 `lastRun.Time + SelectIntervalMin` 算出，不是历史记录；
  选点进行中时该行改为显示当前阶段（如「选点进行中：下载测速中」）。
- **其下是历史记录**，倒序排列（最新在上），格式为 `时间，耗时 N 秒，节点名（地址，延迟，速度）`。
  失败记录显示为 `时间，耗时 N 秒，失败：<原因>` 并标红。
- **只在选点完成后追加一条**。刷新页面、查看日志、切换标签页都不会改动它——
  后端只在追加历史时写 `s.history`，其余时候只读。
- **界面最多显示 `HistoryLimit = 100` 条**（2026-09-20 由 50 上调）。列表本身倒序，
  最新记录天然在顶部，所以不需要额外的滚动定位——这也是那个「自动滚到最新」开关
  被删掉的原因：它想解决的问题根本不存在（2026-09-20 移除）。
- 服务端有两份数据，别混为一谈：
  - **内存 `history []runInfo`** —— 只是「给界面看的窗口」，`appendHistory` 保留最近
    `HistoryLimit` 条，超出后从最早的开始丢，**进程重启即清空**。上限只是防止长时间
    运行时内存无限增长。
  - **`logs/xpilot-select.log`** —— 完整历史，每条选点**追加一行 JSON**（`appendSelectLog`），
    只追加、不轮转、不裁剪，重启后仍可查。（2026-09-20 变更：此前要求纯内存不落盘。）
- `appendSelectLog` 的调用位置**在 `s.mu.Unlock()` 之后**——磁盘 IO 不该阻塞整个服务；
  写失败只往 stderr 打一行，不影响选点流程本身。
- `GET /api/config` 的 `history` 字段返回一份**副本**（`copy` 到新切片），
  避免在锁外序列化时与并发的 `append` 竞争底层数组。

### 当前节点卡片

「运行状态」页第一张卡片显示当前生效的节点，数据来自 `GET /api/config` 的 `current` 字段
（后端从最近一条带实测指标的历史里挑出；历史为空时退回 `lastRun`）：

- **节点名 + 地址**；
- **延迟**（`latencyMs`，按 <150ms 绿 / <300ms 橙 / 其余红着色）；
- **下载速度**（`speedBps`，`fmtSpeed` 转成 B/s ~ GB/s 的可读文本）；
- **延迟趋势迷你图**：纯手绘内联 SVG（不引图表库），取历史里带有效延迟的最多 30 个点画折线 + 面积填充，
  并标注「N 次 · 较首次 ↑/↓ X ms ｜ min~max ms」。

默认节点不做实测，所以延迟/速度为空，显示 `—` 而不是 `0`。

### 后端失联提示

浏览器控制台页面是**常驻**的，而 xpilot 进程可能被关掉、崩掉或端口被占。
这时页面上的数据会停在最后一刻，如果不做处理，用户分不清「程序挂了」还是「操作没生效」。

处理方式是在页面顶部显示一条红色横幅：

- `refresh()` 里对 `/api/config` 的响应做 `resp.ok` 检查（不只是 catch 网络异常），
  非 2xx 也判为失联；
- `act()` 里 `fetch` 抛错（`TypeError: Failed to fetch`）时**除了弹窗提示，还会点亮横幅**，
  并把提示文案换成「连不上后端，进程可能已退出」而不是把裸错误抛给用户；
- SSE 的 `onerror` 也接同一套逻辑，作为兜底（事件流断了说明后端没了）；
- 恢复时由 `refresh()` 成功分支里的 `setOffline(false)` 自动收起横幅，
  不需要刷新页面——点「重试」按钮即可。

区分两类失败很重要：**HTTP 200 + `ok:false`** 是后端正常但拒绝了操作（如「选点正在进行中」），
**不发横幅**；**`fetch` 抛错**才是失联，**发横幅**。两者都会弹 `alert`，但含义不同。

### PAC 随机串机制

`setRegistryPAC` 每次写入 `AutoConfigURL` 都会附加一个新的 12 位随机十六进制参数：

```
http://127.0.0.1:6001/pac?eaf86e4dc923
```

**URL 变化强制浏览器重新拉取 PAC**，否则浏览器会一直用缓存里的旧规则。
所以手改 `html/pac` 之后，点一下控制台的「刷新 PAC」，新规则立刻生效——
不点的话，最迟下一轮选点也会自动带上新随机串。

写入后通过 `wininet.dll` 的 `InternetSetOptionW` 广播 `INTERNET_OPTION_SETTINGS_CHANGED`（39）
和 `INTERNET_OPTION_REFRESH`（37），通知系统立即应用。

---

## 控制台使用说明

<http://127.0.0.1:6001/> 共七个标签页：

| 标签页 | 内容 |
|---|---|
| **运行状态** | 当前节点卡片（节点名/地址 + 延迟 + 下载速度 + 延迟趋势迷你图）、选点状态与阶段、**选点历史列表**（首行固定为待执行时间，其下倒序，**最多 100 条**）、「立即选点」「应用默认节点」按钮 |
| **参数设置** | 全部配置项的表单，灰字显示默认值与允许范围，保存后下一次选点生效 |
| **路由规则** | 两级分流的可视化说明（架构图 + 两层规则表格 + `domainStrategy` 取舍解释） |
| **访问日志** | `logs/xpilot-access.log` 末尾最多 **200** 条记录（上限在后端常量 `logMaxLines`，每 3 秒自动刷新，支持按域名 / IP / 走向过滤）。有截断时统计行会显示「最近 200 条 · 更早记录未显示」，搜索也只在这 200 条内进行 |
| **PAC 规则** | `html/pac` 里的**域名清单**（一行逗号分隔、自动折行）。只呈现 `proxyList` 的域名，注释与 `FindProxyForURL` / `matchAny` 函数体不显示；卡片右上角有搜索框，输入即过滤并高亮命中片段（前端内存筛选，无网络往返）。**单次最多渲染 200 条**，超出部分提示「继续输入缩小范围」，避免长文本重排导致的输入卡顿。文件结构不符时回退为原样显示全文并禁用搜索框 |
| **xray 配置** | `config/config.json` 内容（JSON 自动格式化） |
| **xpilot 参数** | `config/xpilot.json` 内容 |

标题栏三个按钮：

- **刷新 PAC** — 强制浏览器重新拉取 PAC 规则
- **打开注册表** — 定位到系统代理所在的注册表项
- **运行报告** — 新窗口打开最近一次的 HTML 运行报告

---

## 构建与打包

### 前置条件

- Go 1.27 或更高
- Windows（依赖 `syscall` 的 `HideWindow`、`wininet.dll`、`netstat` / `taskkill` / `reg`）

### 一键打包（推荐）

```powershell
pwsh -File build.ps1
```

`build.ps1` 把「停旧进程 → 构建 → 替换 → 起新进程」串成一条流水线：

1. **检查进程**：`Get-Process -Name xpilot`，没有就跳过（不算错）。
2. **杀进程**：`Stop-Process -Force`，然后**轮询最多 10 秒**确认它真的退出了。
   这一步不能省 —— 进程退出与实际释放文件句柄之间有时间差，急着复制会失败。
   超时未退就报错退出，不冒险覆盖。
3. **构建到临时路径**：先输出到 `$env:TEMP\xpilot-build.exe`，
   这样构建失败时目标位置不会留下一个半截文件。
4. **原子替换**：`Move-Item -Force` 覆盖到 `E:\application\我的工具箱\xpilot.exe`。
5. **拉起新进程**：`Start-Process` 启动，并回读 PID 确认真的起来了。

**为什么必须先停再建**：运行中的 exe 会锁住自己的映像文件，
直接 `go build -o` 到目标路径会报 `Access is denied`。
旧脚本正是在这里卡住、需要人工处理，所以才改成自动化。

脚本文件保持 **UTF-8 with BOM + CRLF**（头部有 BOM），
这样 Windows PowerShell 5.1 与 PowerShell 7 都能正确解析其中的中文路径。

### 手动构建

```powershell
# 开发构建（带控制台输出，方便调试）
go build -o xpilot-dev.exe .

# 发布构建（无控制台窗口、去符号、去路径）
go build -trimpath -ldflags "-s -w -H=windowsgui" -o xpilot.exe .
```

### 关于编译参数

| 参数 | 作用 |
|---|---|
| `-trimpath` | 去掉二进制里的本地绝对路径，让构建可复现 |
| `-ldflags "-s -w"` | 去掉符号表与调试信息，体积能小几 MB |
| `-H=windowsgui` | 标记为 GUI 子系统程序，运行时不弹控制台窗口 |

### 注意

- 构建产物约 **62.6 MB**——绝大部分是内嵌的 `geoip.dat`（19.8 MB）+ `geosite.dat`（10.5 MB）+ xray-core 全部协议模块。
- exe 运行时如果被占用，`go build` 会报 `Access is denied`。
  手动构建时先退出正在运行的 xpilot；用 `build.ps1` 则它已代为处理。
- `html/` 下的四个文件（`console.html` / `pac` / `geoip.dat` / `geosite.dat`）在构建时必须齐全，
  否则 `//go:embed` 会编译失败。`pac` 若确实没有，会把内置模板释放出去。

---

## 测试

```powershell
go test -v ./...          # 全部测试
go test -short ./...      # 跳过有副作用的测试
go vet ./                 # 静态检查
gofmt -l .                # 检查格式
```

### 测试清单

| 测试 | 覆盖内容 |
|---|---|
| `TestReportHTML` | 用假数据渲染报告，验证各关键区块（统计、图表、明细表、徽章）都存在 |
| `TestRegistryPAC` | 真实读写 `HKCU` 注册表，验证 PAC 地址拼接与旧随机串剥离 |
| `TestXrayTestAllFakeNodes` | 用假节点跑通 `xrayTestAll` 全流程：实例创建、验证失败路径、实例回收。不依赖外网 |
| `TestParseAccessLines` | 访问日志解析：`->` 与 `>>` 两种箭号、`tcp:` 前缀、嗅探出的 `//域名`、rejected、无 detour、无法识别的行跳过 |
| `TestTailFile` | 文件末尾读取：正常读全、超限截尾并丢弃半行、文件不存在报错 |
| `TestHandleAccessLogLimit` | 访问日志接口的返回上限：只留末尾 `logMaxLines` 条、最新在前、超出时置 `truncated`、少量行不截断、文件缺失返回空列表 |
| `TestAppendHistoryLimit` | 选点历史内存窗口：写 `HistoryLimit+30` 条后只保留最近 `HistoryLimit` 条、末条最新、首条为窗口起点 |
| `TestAppendSelectLog` | 选点历史落盘：写 `HistoryLimit+20` 条后 `logs/xpilot-select.log` 有**全部**行（不受内存上限约束），每行均为合法 JSON 且 `error` / `metrics` 字段完整 |
| `TestMigrateAndRelease` | 旧文件迁移（根目录 / `config/` / `html/` / `logs/` 四个历史位置）+ 内嵌资源释放（`console.html` 每次覆盖，`pac` / geo 资产仅缺失时写） |

### ⚠️ `TestRegistryPAC` 的副作用

这个测试会**真实修改** `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings\AutoConfigURL`，
把系统代理指向 `http://127.0.0.1:6001/pac?<随机串>`。

- 如果 xpilot 正在运行，这不会造成问题（本来就是它该有的值）
- 如果在没运行 xpilot 的机器上跑测试，跑完系统代理会指向一个不存在的 PAC 服务
- 另外，沙箱环境如果拦了 `reg.exe`，这个测试会失败（`AutoConfigURL 读回 ""`）——属于环境限制，不是代码问题

建议在 CI 或非开发环境用 `-short` 跳过，或者后续把这个测试重构为可注入的注册表接口。

---

## 关键设计决策

这一节记录几个**看起来可以更简单、但故意做成现在这样**的地方，避免后来者（包括未来的自己）误改。

### 1. `domainStrategy: AsIs`，而不是 `IPIfNonMatch`

路由规则用的是 `AsIs`——域名连接只按域名规则匹配，不做本地 DNS 解析。

**不用 `IPIfNonMatch` 的原因**：它需要先解析域名再匹配 IP 规则。而国内 ISP 的 DNS
对被墙域名会返回污染地址（例如谷歌的 `203.208.x.x`），这些假 IP 在 `geoip` 库里**属于中国**——
于是国外站点会被「国内 IP → 直连」规则误判为直连，结果就是打不开。

`AsIs` 下未命中域名规则的域名会把域名原样交给出站，由节点服务器负责解析，
既不误判也更快（少一次本地 DNS 往返）。

### 2. 延迟探测用 `github.com`，不用 `gstatic.com`

理由同上：`gstatic.com` 在国内有 Google 边缘节点，直连也能 204，会造成节点「假通过」。
`verifyOutbound` 用 `www.google.com/generate_204` 也是同一个考虑（国内无边缘节点，必然走代理）。

### 3. 每 IP 只占一个候选名额

机场常在一台服务器上开十几个端口。若按纯延迟排序挑候选，名单容易被同一台机器包揽。
`pickCandidates` 按 IP 去重，保证候选来自不同的物理服务器。

同时，**正在使用的节点强制入围**——即使它延迟排名靠后，也要让它和新挑战者同场实测，
避免「没实测过就被换掉」。

### 4. 80% 粘性阈值

`saveBestAndPick` 里，如果当前节点速度达到最优节点的 80%，就保持不动。

**理由**：速度在 80%~100% 之间的节点之间来回切换，用户体验几乎没有差别，
但每次切换都有成本（热替换、连接重建、TLS 握手）。粘性策略把无意义的抖动消掉。

### 5. 端口写死不做成配置项

`10808`（代理）与 `6001`（控制台/PAC）在代码里是常量。这不是偷懒——
PAC 文件内容、注册表 `AutoConfigURL`、用户浏览器配置全都与这两个端口绑定，
开放修改只会让用户有能力把自己搞成一个「代理指向空端口」的状态。
`6001` 被占用时直接退出，同时兼作单实例锁。

### 6. `xraycore.go` 作为单一接入层

全项目只有 `xraycore.go` 直接 `import` `xray-core`。
其余代码操作的是 `xrayInstance` 包装类型。这样升级 xray-core 版本时，
编译报错会集中在这一个文件里，改一个地方就够。

### 7. 实测与生产共用 `buildXrayConfig`

测速实例和运行实例用同一个函数生成配置，只靠 `withRouting` 参数区分。
保证「测出来的速度」就是「用起来的速度」——如果两份配置生成逻辑分开维护，
很容易出现测速走的路由和实际不一样的情况。

### 8. 配置校验只做「保存时拦截」，不做「启动时拦截」

`loadConfig` **不**调用 `validateConfig`。配置文件被改坏时，宁可带着可疑参数跑起来，
让用户能在网页上看到实际值并改回，也不要因为一个参数不合法就拒绝启动
（那样用户只能手改文件，更麻烦）。

非法值会在下一次保存时被后端拒绝，控制台的「参数设置」页也会显示当前实际值。

### 9. `hideWin` 给所有子进程加隐藏窗口标志

xpilot 是无控制台的后台程序。不加 `CREATE_NO_WINDOW`（`0x08000000`）的话，
每次它调用 `reg` / `netstat` / `taskkill` 都会**弹出一个黑色控制台窗口**——
在 PAC 刷新这种会被频繁触发的路径上，那画面相当难看。

---

## 已知限制与注意事项

### 功能限制

- **只支持 vless 节点**。`buildXrayConfig` 明确拒绝其他协议：`暂只支持 vless 节点，当前协议: %s`。
  订阅解析（`extractAddrPort`）支持 vmess/ss/ssr 的地址提取，但最终构建配置时会失败。
- **只支持 Windows**。依赖 `wininet.dll`、`HKCU` 注册表、`netstat` / `taskkill` / `reg`。
- **只监听回环地址**。控制台与 PAC 服务都绑定 `127.0.0.1`，不接受局域网访问。
- **无鉴权**。任何本机进程都能访问控制台并修改参数。这是单机工具的有意取舍，
  但也意味着不要把 `6001` 端口通过内网穿透暴露出去。
- **访问日志不设保留上限，会持续增长**。按用户要求「一直存」，没有清理逻辑。
  重度使用（比如开着视频流）每天几万条是正常的，单条约 100 字节，
  一年量级在几十到几百 MB ——对现代硬盘不算什么，但心里要有数。
  想瘦身就直接删掉 `logs/xpilot-access.log`，程序下次写入时会重新建。
- **选点历史文件也不轮转**。`logs/xpilot-select.log` 每条选点一行（约 350 字节），
  按 60 分钟间隔算一年也就 5 MB 上下，量级远小于访问日志，同样没有清理逻辑。
  注意：**删掉它不会重建空的也不会报错**，但界面上的历史仍然来自内存、不受影响。

### 运行注意

- **端口冲突即退出**。`6001` 被其他程序占用时进程直接退出，日志里会写明原因。
  排查：`netstat -ano | findstr :6001`。
- **首次运行会在注册表写入 PAC 配置**。这是程序的核心工作机制（系统代理 PAC 模式），
  不是可选行为。如果不想让它改注册表，需要在 PAC 系统层面另找方案。
- **`geoip.dat` / `geosite.dat` 丢失会导致主实例起不来**。它们在 `html/` 下，
  若被误删，重新运行 exe 会自动释放（因为内嵌了）。
- **删掉 `html/console.html` 不会有事**——每次启动都会用内嵌版本覆盖回去。
- **不要手改 `config/config.json`**。它是每次选点落地时自动生成的产物，
  你的修改会在下次选点时被覆盖。要改路由规则请改 `buildXrayConfig`。

### 代码层面待改进项

这些是在代码评审中确认过、但尚未处理的问题，记录在此以免遗忘：

1. **`TestRegistryPAC` 有真实副作用**（见[测试](#测试)一节），建议用 `-short` 跳过或重构为可注入接口。

> 已修复：**访问日志的走向着色在部分情况下失效**。
> 两处原因：① 不同 xray 版本写入的 `->` 与 `>>` 两种箭号，正则原先只认前者，
> 导致整串 `mixed-in >> proxy` 落到前端 `OUTCLS` 映射不上，显示成灰色「未知」；
> ② `rejected` 行覆盖 `out` 字段同样映射不上。
> 现解析改为按**最后一个 `>`** 切分（两种箭号通吃），并顺带剥掉
> `from` / 目标上的 `tcp:` `udp:` 协议前缀与嗅探成功时的 `//` 前缀。

> 已修复：`handleDefaultNode` 的数据竞争。原先 `s.running = true` 放在 goroutine 内部，
> HTTP 响应会先于置位发出，前端拉快照仍看到「空闲」→ 按钮不置灰 → 用户以为没点上而连点 →
> 两次请求都通过 `running` 检查并发触发 `applyWinner`。
> 现已对齐 `startSelection` 的时序（同步置位 + 推 `stage` 事件），
> 前端同时改为**乐观置灰**并去掉了 `act()` 里硬等的 500ms。

---

## 附：常见问题排查

**Q: 控制台打不开？**
检查 `%AppData%\xpilot\logs\xpilot.log` 里有没有 `HTTP 服务启动失败`。
多半是 `6001` 被占用（可能 xpilot 已经在跑了）。

**Q: 选点一直失败？**
看日志里的具体阶段：
- `未配置订阅地址` → 去参数设置页填订阅
- `没有获取到任何节点（N 个订阅失败）` → 订阅地址失效或被墙，用浏览器验证一下订阅 URL 能否访问
- `所有节点均无法连通` → 关掉 `tcpTest` 试试（可能本机网络环境问题）
- `没有任何节点通过 xray 实测` → 节点全部协议验证失败，检查 `proto` 过滤设置是否过严

**Q: 选点成功了但上不了外网？**
1. 看控制台「访问日志」页，确认目标域名命中的是 `proxy` 还是 `direct`
2. 检查系统代理是否真的生效：控制台「路由规则」页会显示当前 `AutoConfigURL`
3. 手动访问 `http://127.0.0.1:6001/pac` 确认 PAC 服务正常返回内容
4. 若用 Firefox，确认它单独配置了 PAC URL（Firefox 默认不跟随系统代理）

**Q: 网站明明能访问，但访问日志里就是没有它？**

先分清两种情况，否则容易查错方向：

1. **该域名走了 PAC 的 `DIRECT`** → 流量根本没进 xray，日志里当然没有。
   先在 `html/pac` 里确认它没被杀进直连规则。
2. **日志本身没在写**。判断方法是：换一个**确定需要走代理**的域名
   （如 `google.com`）访问一次，再看日志有没有新增。
   如果连它也没有 → 不是域名的问题，是写入通道坏了。

> 自查手段：`Get-Item "$env:AppData\xpilot\logs\xpilot-access.log" | Select LastWriteTime`
> 看最后写入时间。光看「有没有那一条记录」分不清上面两种情况。

**Q: 某些国内网站变慢了？**
说明它被 PAC 交给了 xray，又被 xray 路由判给了 proxy。
去控制台「访问日志」页搜那个域名，然后在 `html/pac` 里加一条 `DIRECT` 规则，
点「刷新 PAC」生效。

**Q: 想让某个国外网站直连？**
同上，在 `html/pac` 里把它加进 `DIRECT` 分支即可。PAC 的优先级高于 xray 路由——
PAC 判 DIRECT 的流量根本不会进入 xray。

**Q: 控制台页面顶部出现红色「与 xpilot 后端失去连接」横幅？**
说明 xpilot 进程已经不在了（`fetch` 连不上 `127.0.0.1:6001`）。按提示确认：

1. 任务管理器（或 `tasklist | findstr xpilot`）看 `xpilot.exe` 是否还在；
2. `netstat -ano | findstr :6001` 看端口是否在监听——**没有 LISTENING 就说明进程已退出**；
3. 重新双击 `xpilot.exe`，页面不需要刷新，点横幅右侧的「重试」即可恢复。

此时页面上的数据是停住的旧值（按钮灰着、指标显示 `—`、历史只有「待执行（即将开始）」）
都是正常的降级表现，不是功能坏了。进程起来后所有数据会自动回来。

**Q: 弹「操作失败：TypeError: Failed to fetch」是什么意思？**
同上，就是连不上后端。但如果弹的是「操作失败：选点正在进行中」这类后端返回的文案，
说明后端活着、只是拒绝了这次操作（防重复触发），页面数据依然有效。
