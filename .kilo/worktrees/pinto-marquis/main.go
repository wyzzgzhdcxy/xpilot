// xpilot 常驻代理管家：拉取机场订阅，过滤协议、按 地址:端口 去重，TCP 连通测试后
// 写出 node.list.txt；然后给每个节点生成独立 xray 配置、各起一个 xray-core 进程用
// 不同本地端口实测：先并发验证协议连通并测延迟，再对候选串行实测下载速度。实测与
// 最终运行用同一份配置生成逻辑，候选保存到 node.best.txt，下载速度最优者作为最终
// 节点。节点切换采用蓝绿方式：先在从端口起新节点并验证，PAC 切到从端口，原地换主
// 端口节点后再切回，全程不断网。
// 进程以常驻服务方式运行：内置 Web 控制台（状态/参数/路由规则/访问日志）、PAC HTTP 服务
// （6001 端口的 /pac）、定时选点循环与掉线自愈。exe 是单文件：html/ 下的资源（控制台
// 页面、PAC、geoip/geosite 资产）全部内嵌，首次运行释放到用户数据目录 %AppData%\xpilot，
// 配置写在 config/ 子目录，日志在 logs/ 子目录，全程无需联网准备资源。
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultPacMain = "http://127.0.0.1:6001/pac"
)

var httpClient = &http.Client{
	Timeout:   30 * time.Second,
	Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
}

func fetchBody(rawURL string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	// v2rayN UA 保证机场返回 base64 格式而不是 clash YAML
	req.Header.Set("User-Agent", "v2rayN/6.45")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

var base64Encodings = []*base64.Encoding{
	base64.StdEncoding,
	base64.URLEncoding,
	base64.RawStdEncoding,
	base64.RawURLEncoding,
}

func b64Decode(s string) ([]byte, bool) {
	s = strings.TrimSpace(s)
	for _, enc := range base64Encodings {
		if b, err := enc.DecodeString(s); err == nil && len(b) > 0 {
			return b, true
		}
	}
	return nil, false
}

func splitLines(s string) []string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// decodeSubscription 把订阅响应体还原成一行一条的节点链接。
func decodeSubscription(body string) []string {
	body = strings.TrimSpace(body)
	// 明文链接列表（base64 字母表里不可能出现 "://"）
	if strings.Contains(body, "://") {
		return splitLines(body)
	}
	// 兼容多行 base64：去掉全部空白后再解码
	compact := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, body)
	if decoded, ok := b64Decode(compact); ok {
		return splitLines(string(decoded))
	}
	return nil
}

// cutAt 在最早的分隔符处截断。
func cutAt(s string, seps ...byte) string {
	for _, sep := range seps {
		if i := strings.IndexByte(s, sep); i >= 0 {
			s = s[:i]
		}
	}
	return s
}

// extractAddrPort 从各种节点分享链接里取出服务器地址和端口。
func extractAddrPort(link string) (host, port string, ok bool) {
	lower := strings.ToLower(link)

	switch {
	case strings.HasPrefix(lower, "vmess://"):
		payload := cutAt(link[len("vmess://"):], '#')
		if b, dec := b64Decode(payload); dec {
			var m struct {
				Add  string      `json:"add"`
				Port interface{} `json:"port"`
			}
			if json.Unmarshal(b, &m) == nil && m.Add != "" && m.Port != nil {
				return m.Add, fmt.Sprintf("%v", m.Port), true
			}
		}
		return "", "", false

	case strings.HasPrefix(lower, "ssr://"):
		payload := cutAt(link[len("ssr://"):], '#', '/')
		if b, dec := b64Decode(payload); dec {
			parts := strings.Split(string(b), ":")
			if len(parts) >= 2 {
				return parts[0], parts[1], true
			}
		}
		return "", "", false

	case strings.HasPrefix(lower, "ss://"):
		rest := cutAt(link[len("ss://"):], '#')
		// SIP002: base64(method:password)@host:port
		if i := strings.LastIndex(rest, "@"); i >= 0 {
			if h, p, err := net.SplitHostPort(rest[i+1:]); err == nil {
				return h, p, true
			}
		}
		// 旧格式: base64(method:password@host:port)
		if b, dec := b64Decode(rest); dec {
			if i := strings.LastIndex(string(b), "@"); i >= 0 {
				if h, p, err := net.SplitHostPort(string(b)[i+1:]); err == nil {
					return h, p, true
				}
			}
		}
		return "", "", false

	default:
		// 通用格式 scheme://userinfo@host:port?query#fragment
		i := strings.Index(link, "://")
		if i < 0 {
			return "", "", false
		}
		rest := cutAt(link[i+3:], '#', '?', '/')
		if j := strings.LastIndex(rest, "@"); j >= 0 {
			rest = rest[j+1:]
		}
		if h, p, err := net.SplitHostPort(rest); err == nil {
			return h, p, true
		}
		// 兜底：可能是 URL 编码过的地址
		if u, err := url.Parse(link); err == nil && u.Host != "" {
			if h, p, err := net.SplitHostPort(u.Host); err == nil {
				return h, p, true
			}
		}
		return "", "", false
	}
}

// tcpStat 是单个节点的 TCP 连通测试结果（与输入链接按顺序对应）。
type tcpStat struct {
	addr string
	ok   bool
	cost time.Duration
	err  error
}

// testAndFilter 对每个节点的 地址:端口 做 TCP 连通测试（等价 telnet），只保留能连上的。
// 结果按输入顺序打印，返回按原顺序排列的连通节点及全部节点的测试结果。
func testAndFilter(links []string, timeout time.Duration, workers int) ([]string, []tcpStat) {
	type dialResult struct {
		addr string
		ok   bool
		cost time.Duration
		err  error
	}
	results := make([]dialResult, len(links))
	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)

	for i, link := range links {
		wg.Add(1)
		go func(i int, link string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			addr := ""
			if h, p, ok := extractAddrPort(link); ok {
				addr = net.JoinHostPort(h, p)
			}
			if addr == "" {
				results[i] = dialResult{err: errors.New("无法解析地址")}
				return
			}
			start := time.Now()
			conn, err := net.DialTimeout("tcp", addr, timeout)
			cost := time.Since(start)
			if err == nil {
				conn.Close()
			}
			results[i] = dialResult{addr: addr, ok: err == nil, cost: cost, err: err}
		}(i, link)
	}
	wg.Wait()

	kept := make([]string, 0, len(links))
	stats := make([]tcpStat, len(links))
	for i, r := range results {
		stats[i] = tcpStat{addr: r.addr, ok: r.ok, cost: r.cost, err: r.err}
		if r.ok {
			fmt.Printf("  [连通] %-42s %v\n", r.addr, r.cost.Round(time.Millisecond))
			kept = append(kept, links[i])
		} else {
			fmt.Printf("  [失败] %-42s %v\n", r.addr, r.err)
		}
	}
	return kept, stats
}

// matchProto 判断节点链接是否属于指定协议（proto 为空表示不过滤）。
func matchProto(link, proto string) bool {
	if proto == "" {
		return true
	}
	i := strings.Index(link, "://")
	return i > 0 && strings.EqualFold(link[:i], proto)
}

func protoName(p string) string {
	if p == "" {
		return "全部"
	}
	return p
}

// ---------- xray-core 实测 ----------

// nodeName 取节点链接的展示名称（#fragment，没有则用 host:port）。
func nodeName(link string) string {
	u, err := url.Parse(link)
	if err != nil || u.Hostname() == "" {
		return cutAt(link, '#')
	}
	if u.Fragment != "" {
		return u.Fragment
	}
	return net.JoinHostPort(u.Hostname(), u.Port())
}

func formatSpeed(bps float64) string {
	switch {
	case bps >= 1e9:
		return fmt.Sprintf("%.2f GB/s", bps/1e9)
	case bps >= 1e6:
		return fmt.Sprintf("%.2f MB/s", bps/1e6)
	case bps >= 1e3:
		return fmt.Sprintf("%.1f KB/s", bps/1e3)
	default:
		return fmt.Sprintf("%.0f B/s", bps)
	}
}

// freePort 找一个当前空闲的本地端口（有极小被抢占的可能，实测场景可接受）。
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func addrOf(r nodeResult) string {
	if r.addr == "" {
		return "(无法解析地址)"
	}
	return r.addr
}

type testNode struct {
	link string
	port int
	inst *xrayInstance
}

// stopTestNode 关闭实测用的内存 xray 实例。
func stopTestNode(n *testNode) {
	if n != nil && n.inst != nil {
		n.inst.Close()
		n.inst = nil
	}
}

// startTestNode 为单个节点在随机端口上创建一个内存中的临时 xray 实例。
// 测速实例不带分流路由（测试只需走节点，也免去 geoip 数据的加载开销）。
func startTestNode(link string) (*testNode, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	cfgJSON, err := buildXrayConfig(link, port, false, "none", "none")
	if err != nil {
		return nil, err
	}
	inst, err := startXrayInstance(cfgJSON)
	if err != nil {
		return nil, err
	}
	// 等入站监听就绪
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for i := 0; i < 30; i++ {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return &testNode{link: link, port: port, inst: inst}, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	inst.Close()
	return nil, fmt.Errorf("实例端口 %d 未就绪", port)
}

// proxyClient 返回走指定本地 xray 端口的 http 客户端（mixed 入站，socks5/http 同端口）。
func proxyClient(port int, timeout time.Duration) *http.Client {
	pu := &url.URL{Scheme: "socks5", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: http.ProxyURL(pu)},
	}
}

// probeLatency 通过节点实例请求探测地址，验证协议连通并测延迟。
// 探测目标必须是国内无法直连的站点：gstatic.com 在国内有 Google 中国边缘节点
// （203.208.x.x，命中 geoip:cn），会被路由规则判成直连绕过节点，造成假通过。
// 默认用 github.com——延迟排名直接对准最关心的 GitHub 体验。
func probeLatency(port int, probeURL string, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	resp, err := proxyClient(port, timeout).Get(probeURL)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	return time.Since(start), nil
}

// probeDownload 通过节点实例下载测速文件，返回平均速度（字节/秒）。
// 下载到 downloadSize 上限或超时为止，按实际字节数/耗时计算。
func probeDownload(port int, serverURL string, downloadSize int, timeout time.Duration) (float64, error) {
	start := time.Now()
	resp, err := proxyClient(port, timeout).Get(serverURL)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, int64(downloadSize)))
	elapsed := time.Since(start)
	if n == 0 {
		return 0, errors.New("没有下载数据")
	}
	return float64(n) / elapsed.Seconds(), nil
}

type xrayTestOpts struct {
	onStage      func(string)
	serverURL    string
	latencyURL   string // 延迟探测地址（决定按哪条线路排名）
	currentAddr  string // 当前正在使用的节点 服务器:端口（保底进入决赛候选）
	maxLatency   time.Duration
	dlTimeout    time.Duration
	overall      time.Duration
	downloadSize int
	top          int
}

type nodeResult struct {
	link      string
	name      string
	addr      string
	port      int
	tn        *testNode
	latencyMs float64 // 验证失败为 +Inf
	speedBps  float64
	speedText string
	errText   string // 验证/下载失败原因（供报告展示）
	candidate bool   // 进入下载实测的决赛候选
}

// xrayTestAll 对全部候选节点做 xray-core 实测：每个节点一个内存实例、独立本地
// 端口；先并发验证协议连通并测延迟，再对延迟最优的前 top 个串行实测下载速度
// （串行避免共享本地带宽互相干扰）。返回按延迟升序的结果，只有前 top 个带速度
// 数据。实测与最终运行用的是同一份配置生成逻辑 buildXrayConfig。
func xrayTestAll(links []string, opt xrayTestOpts) ([]nodeResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), opt.overall)
	defer cancel()

	fmt.Printf("\n开始 xray-core 实测：%d 个节点，每节点独立内存实例；验证超时 %v，下载超时 %v/节点\n",
		len(links), opt.maxLatency, opt.dlTimeout)

	nodes := make([]*testNode, 0, len(links))
	if opt.onStage != nil {
		opt.onStage("创建测试实例中")
	}
	defer func() {
		for _, n := range nodes {
			stopTestNode(n)
		}
	}()

	for _, link := range links {
		n, err := startTestNode(link)
		if err != nil {
			fmt.Printf("  [失败] %-42s %v\n", nodeName(link), err)
			continue
		}
		nodes = append(nodes, n)
	}

	if opt.onStage != nil {
		opt.onStage("延迟验证中")
	}
	// 并发验证协议连通并测延迟：每节点探测 3 次，≥2 次通过取中位数（2 次取较差的），
	// 只通过 0-1 次视为不稳定节点淘汰
	var wg sync.WaitGroup
	lat := make([]time.Duration, len(nodes))
	lerr := make([]error, len(nodes))
	for i, n := range nodes {
		wg.Add(1)
		go func(i int, n *testNode) {
			defer wg.Done()
			var ds []time.Duration
			var lastErr error
			for k := 0; k < 3 && ctx.Err() == nil; k++ {
				d, err := probeLatency(n.port, opt.latencyURL, opt.maxLatency)
				if err == nil {
					ds = append(ds, d)
				} else {
					lastErr = err
				}
			}
			if len(ds) < 2 {
				lerr[i] = fmt.Errorf("3 次验证仅通过 %d 次：%v", len(ds), strings.TrimSpace(lastErr.Error()))
			} else {
				sort.Slice(ds, func(a, b int) bool { return ds[a] < ds[b] })
				lat[i] = ds[len(ds)/2]
			}
			if lerr[i] != nil {
				stopTestNode(n) // 死节点立即回收进程
			}
		}(i, n)
	}
	wg.Wait()

	rows := make([]nodeResult, 0, len(nodes))
	for i, n := range nodes {
		r := nodeResult{link: n.link, name: nodeName(n.link), port: n.port, tn: n}
		if h, p, ok := extractAddrPort(n.link); ok {
			r.addr = net.JoinHostPort(h, p)
		}
		if lerr[i] != nil {
			r.latencyMs = math.Inf(1)
			r.errText = "验证失败：" + strings.TrimSpace(lerr[i].Error())
			fmt.Printf("  [失败] %-42s %s\n", addrOf(r), strings.TrimSpace(lerr[i].Error()))
		} else {
			r.latencyMs = float64(lat[i].Microseconds()) / 1000
			fmt.Printf("  [通过] %-42s %.0fms\n", addrOf(r), r.latencyMs)
		}
		rows = append(rows, r)
	}

	// 挑决赛候选并逐个实测下载速度（串行避免共享本地带宽互相干扰）
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].latencyMs < rows[j].latencyMs })
	pick := pickCandidates(rows, opt.top, opt.currentAddr)
	if len(pick) > 0 {
		fmt.Printf("\n对 %d 个候选节点串行实测下载速度（%s）...\n", len(pick), opt.serverURL)
		if opt.onStage != nil {
			opt.onStage("下载测速中")
		}
	}
	for _, i := range pick {
		r := &rows[i]
		if ctx.Err() != nil {
			r.errText = "整体超时，未实测下载"
			continue
		}
		r.candidate = true
		sp, err := probeDownload(r.port, opt.serverURL, opt.downloadSize, opt.dlTimeout)
		if err != nil {
			r.errText = "下载失败：" + strings.TrimSpace(err.Error())
			fmt.Printf("  [失败] %-42s %s\n", addrOf(*r), strings.TrimSpace(err.Error()))
		} else {
			r.speedBps = sp
			r.speedText = formatSpeed(sp)
			fmt.Printf("  [通过] %-42s %s\n", addrOf(*r), r.speedText)
		}
		stopTestNode(r.tn) // 测完即回收
	}
	return rows, nil
}

// pickCandidates 从按延迟升序的结果里挑决赛候选：每个 IP 只占一个名额，
// 避免同一台服务器的多个端口包揽候选；正在使用的节点（若存活）保底入围。
func pickCandidates(rows []nodeResult, top int, currentAddr string) []int {
	key := func(addr string) string {
		if h, _, err := net.SplitHostPort(addr); err == nil {
			return strings.ToLower(h)
		}
		return strings.ToLower(addr)
	}
	pick := make([]int, 0, top)
	seen := make(map[string]bool)
	for i := range rows {
		if len(pick) >= top || math.IsInf(rows[i].latencyMs, 1) {
			break
		}
		k := key(rows[i].addr)
		if seen[k] {
			continue
		}
		seen[k] = true
		pick = append(pick, i)
	}
	// 当前使用中的节点保底进入候选，让它和挑战者同场实测
	if cur := key(currentAddr); cur != "" && !seen[cur] {
		for i := range rows {
			if !math.IsInf(rows[i].latencyMs, 1) && key(rows[i].addr) == cur {
				pick = append(pick, i)
				break
			}
		}
	}
	sort.Ints(pick)
	return pick
}

// saveBestAndPick 打印实测结果汇总，把决赛候选节点链接写入 node.best.txt，返回最终节点。
// 规则：实测下载速度最快者胜出；但正在使用的节点若速度不低于最优的 80%，保持不动，
// 避免近优节点之间来回切换造成无谓重启。
func saveBestAndPick(rows []nodeResult, bestDir, currentAddr string) (nodeResult, bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].latencyMs != rows[j].latencyMs {
			return rows[i].latencyMs < rows[j].latencyMs
		}
		return rows[i].speedBps > rows[j].speedBps
	})

	fmt.Println("\n实测结果汇总（按延迟排序，下载速度只实测了候选节点）：")
	fmt.Printf("%-4s %-40s %-28s %-10s %s\n", "排名", "节点", "服务器", "延迟", "下载速度")
	for i, r := range rows {
		lat := "超时"
		if !math.IsInf(r.latencyMs, 1) {
			lat = fmt.Sprintf("%.0fms", r.latencyMs)
		}
		speed := r.speedText
		if speed == "" {
			speed = "-"
		}
		fmt.Printf("%-4d %-40s %-28s %-10s %s\n", i+1, r.name, addrOf(r), lat, speed)
	}

	var live []nodeResult
	bestIdx := -1
	for i, r := range rows {
		if math.IsInf(r.latencyMs, 1) {
			continue
		}
		live = append(live, r)
		if r.speedBps > 0 && (bestIdx < 0 || r.speedBps > rows[bestIdx].speedBps) {
			bestIdx = i
		}
	}
	if len(live) == 0 {
		return nodeResult{}, false
	}

	if best := candidateLinks(rows); len(best) > 0 {
		bestPath := filepath.Join(bestDir, "node.best.txt")
		if err := os.WriteFile(bestPath, []byte(strings.Join(best, "\n")+"\n"), 0o644); err != nil {
			fmt.Printf("写出最优节点文件失败: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\n%d 个候选节点已保存到 %s\n", len(best), bestPath)
	}

	winner := live[0]
	keepNote := ""
	if bestIdx >= 0 {
		winner = rows[bestIdx]
		if currentAddr != "" {
			for _, r := range live {
				if r.speedBps > 0 && strings.EqualFold(r.addr, currentAddr) {
					if pct := 100 * r.speedBps / winner.speedBps; pct >= 80 {
						winner = r
						keepNote = fmt.Sprintf("当前节点速度达到最优的 %.0f%%，保持不变", pct)
					}
					break
				}
			}
		}
	}
	if keepNote != "" {
		fmt.Println(keepNote)
	}
	wspeed := winner.speedText
	if wspeed == "" {
		wspeed = "未测出速度"
	}
	fmt.Printf("最终节点: %s（延迟 %.0fms，下载速度 %s）\n", winner.name, winner.latencyMs, wspeed)
	return winner, true
}

func candidateLinks(rows []nodeResult) []string {
	var links []string
	for _, r := range rows {
		if r.candidate {
			links = append(links, r.link)
		}
	}
	return links
}

// currentNodeAddr 从现有 config.json 里读出正在使用的节点，返回 "服务器:端口"。
func currentNodeAddr(cfgDir string) string {
	data, err := os.ReadFile(filepath.Join(cfgDir, "config.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Outbounds []struct {
			Protocol string `json:"protocol"`
			Settings struct {
				Vnext []struct {
					Address string `json:"address"`
					Port    int    `json:"port"`
				} `json:"vnext"`
			} `json:"settings"`
		} `json:"outbounds"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return ""
	}
	for _, ob := range cfg.Outbounds {
		if ob.Protocol == "vless" && len(ob.Settings.Vnext) > 0 {
			v := ob.Settings.Vnext[0]
			return net.JoinHostPort(v.Address, strconv.Itoa(v.Port))
		}
	}
	return ""
}

// ---------- xray 配置生成与启动 ----------

// buildXrayConfig 把节点链接转成 xray-core 配置（mixed 混合入站，reality/tls 出站）。
// withRouting=true 时带国内直连分流规则（生产实例用）；测速实例传 false，
// 省去 geoip/geosite 数据加载，测试流量全部走节点。
func buildXrayConfig(link string, port int, withRouting bool, accessLog, errorLog string) ([]byte, error) {
	u, err := url.Parse(link)
	if err != nil || u.Hostname() == "" || u.Port() == "" {
		return nil, fmt.Errorf("无法解析节点链接: %s", cutAt(link, '#'))
	}
	if !strings.EqualFold(u.Scheme, "vless") {
		return nil, fmt.Errorf("暂只支持 vless 节点，当前协议: %s", u.Scheme)
	}
	q := u.Query()
	nodePort, _ := strconv.Atoi(u.Port())

	user := map[string]interface{}{"id": u.User.Username(), "encryption": "none"}
	if flow := q.Get("flow"); flow != "" {
		user["flow"] = flow
	}
	outbound := map[string]interface{}{
		"tag":      "proxy",
		"protocol": "vless",
		"settings": map[string]interface{}{
			"vnext": []interface{}{map[string]interface{}{
				"address": u.Hostname(),
				"port":    nodePort,
				"users":   []interface{}{user},
			}},
		},
	}

	stream := map[string]interface{}{}
	network := strings.ToLower(q.Get("type"))
	if network == "" {
		network = "tcp"
	}
	stream["network"] = network
	security := strings.ToLower(q.Get("security"))
	stream["security"] = security
	switch security {
	case "reality":
		rs := map[string]interface{}{"serverName": q.Get("sni"), "publicKey": q.Get("pbk")}
		if fp := q.Get("fp"); fp != "" {
			rs["fingerprint"] = fp
		}
		if sid := q.Get("sid"); sid != "" {
			rs["shortId"] = sid
		}
		stream["realitySettings"] = rs
	case "tls":
		ts := map[string]interface{}{"serverName": q.Get("sni")}
		if fp := q.Get("fp"); fp != "" {
			ts["fingerprint"] = fp
		}
		if q.Get("insecure") == "1" || strings.EqualFold(q.Get("insecure"), "true") {
			ts["allowInsecure"] = true
		}
		stream["tlsSettings"] = ts
	}
	switch network {
	case "ws":
		ws := map[string]interface{}{"path": q.Get("path")}
		if h := q.Get("host"); h != "" {
			ws["headers"] = map[string]interface{}{"Host": h}
		}
		stream["wsSettings"] = ws
	case "grpc":
		stream["grpcSettings"] = map[string]interface{}{"serviceName": q.Get("serviceName")}
	}
	outbound["streamSettings"] = stream

	// access 日志固定关闭；error 日志写入指定文件（为空则输出到进程 stdout = xpilot.log）
	logCfg := map[string]interface{}{"loglevel": "warning", "access": accessLog, "error": errorLog}
	if errorLog == "" {
		logCfg["error"] = "none"
	}
	cfg := map[string]interface{}{
		"log": logCfg,
		"inbounds": []interface{}{
			map[string]interface{}{
				"tag": "mixed-in", "listen": "127.0.0.1", "port": port,
				"protocol": "mixed",
				"settings": map[string]interface{}{"auth": "noauth", "udp": true},
				"sniffing": map[string]interface{}{"enabled": true, "destOverride": []string{"http", "tls", "quic"}},
			},
		},
		"outbounds": []interface{}{
			outbound,
			map[string]interface{}{"tag": "direct", "protocol": "freedom"},
			map[string]interface{}{"tag": "block", "protocol": "blackhole"},
		},
	}
	if withRouting {
		cfg["routing"] = map[string]interface{}{
			// AsIs：域名连接只按域名规则匹配，不做本地 DNS 解析。
			// 不能用 IPIfNonMatch —— 国内 ISP DNS 对国外域名的污染结果（如谷歌 203.208.x.x）
			// 会被 geoip 数据库归为中国 IP，导致国外站点被误判直连而打不开。
			"domainStrategy": "AsIs",
			"rules": []interface{}{
				// 内网/本机 → 直连
				map[string]interface{}{
					"type": "field", "ip": []string{"geoip:private"},
					"outboundTag": "direct",
				},
				// 国内域名 → 直连
				map[string]interface{}{
					"type": "field", "domain": []string{"geosite:cn"},
					"outboundTag": "direct",
				},
				// 国内 IP（按 IP 直连的目标）→ 直连
				map[string]interface{}{
					"type": "field", "ip": []string{"geoip:cn"},
					"outboundTag": "direct",
				},
				// 其余（国外域名/国外 IP）→ 走第一个出站 proxy
			},
		}
	}
	return json.MarshalIndent(cfg, "", "  ")
}

// hideWin 给子进程加隐藏窗口标志。xpilot 是无控制台的后台程序，
// 不加的话每次调用 reg/netstat/taskkill 都会弹出一个黑色控制台窗口。
func hideWin(c *exec.Cmd) *exec.Cmd {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	return c
}

// killPortListeners 结束占用指定端口监听的进程（用于重启旧的 xray 实例）。
func killPortListeners(port int) {
	out, err := hideWin(exec.Command("netstat", "-ano")).Output()
	if err != nil {
		return
	}
	killed := make(map[string]bool)
	for _, ln := range strings.Split(string(out), "\n") {
		if !strings.Contains(ln, "LISTENING") {
			continue
		}
		if !strings.Contains(ln, fmt.Sprintf(":%d ", port)) && !strings.Contains(ln, fmt.Sprintf(":%d]", port)) {
			continue
		}
		fields := strings.Fields(ln)
		if len(fields) == 0 {
			continue
		}
		pid := fields[len(fields)-1]
		if pid == "0" || killed[pid] {
			continue
		}
		killed[pid] = true
		hideWin(exec.Command("taskkill", "/F", "/PID", pid)).Run()
	}
}

// portListening 检查本地端口是否已有进程在监听。
func portListening(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 500*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// ---------- 实例编排（蓝绿切换） ----------

// waitPortFree 等待端口释放（杀掉旧进程后通常几十毫秒内完成）。
func waitPortFree(port int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !portListening(port) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// verifyOutbound 通过指定本地端口做代理出网检查。
// 用国内无法直连的 www.google.com：gstatic.com 有国内边缘节点，直连也能 204，会假通过。
func verifyOutbound(port int) error {
	resp, err := proxyClient(port, 15*time.Second).Get("https://www.google.com/generate_204")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ---------- 系统代理 PAC ----------

const regProxyKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`

const pacDrainPeriod = 2 * time.Second // PAC 切换后留给客户端收尾旧连接的时间

// readPacURL 读当前注册表里的 AutoConfigURL。
func readPacURL() string {
	out, err := hideWin(exec.Command("reg", "query", regProxyKey, "/v", "AutoConfigURL")).Output()
	if err != nil {
		return ""
	}
	f := strings.Fields(string(out))
	if len(f) >= 3 {
		return f[len(f)-1]
	}
	return ""
}

// randHex 生成 n 位随机十六进制字符串。
func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// pacRole 判断当前 AutoConfigURL 指向主还是从 PAC（忽略随机参数）。
// 返回 "main"、"slave"，无法识别（被删/被改成无关值）时返回 ""。

// setRegistryPAC 把 AutoConfigURL 指向指定 PAC 并广播刷新。
// 每次写入都附加新的随机参数：URL 变化强制浏览器重新拉取 PAC，
// 这样手动修改过的 PAC 规则（加分流等）最迟在下一轮运行就会生效。
func setRegistryPAC(pacURL string) string {
	// 剥掉可能残留的旧参数，避免出现连续两个问号
	if i := strings.IndexByte(pacURL, '?'); i >= 0 {
		pacURL = pacURL[:i]
	}
	full := pacURL + "?" + randHex(12)
	hideWin(exec.Command("reg", "add", regProxyKey, "/v", "AutoDetect", "/t", "REG_DWORD", "/d", "0", "/f")).Run()
	hideWin(exec.Command("reg", "add", regProxyKey, "/v", "AutoConfigURL", "/t", "REG_SZ", "/d", full, "/f")).Run()
	wininet := syscall.NewLazyDLL("wininet.dll")
	setOpt := wininet.NewProc("InternetSetOptionW")
	setOpt.Call(0, 39, 0, 0) // INTERNET_OPTION_SETTINGS_CHANGED
	setOpt.Call(0, 37, 0, 0) // INTERNET_OPTION_REFRESH
	fmt.Printf("系统代理 PAC 已切换: %s\n", full)
	return full
}

// main 是常驻入口：启动即拉起 Web 控制台、PAC 服务与调度循环，之后一直在后台运行。
func main() {
	runReport.started = time.Now()
	exePath, err := os.Executable()
	if err != nil {
		fmt.Printf("无法获取程序路径: %v\n", err)
		os.Exit(1)
	}
	exeDir := filepath.Dir(exePath)

	// exe 是单文件、任意位置可运行：所有运行期文件（html/、config/、logs/、geo 资产）
	// 都集中在用户数据目录，首次运行把旧 exe 目录里的文件迁移过来并释放内嵌资源。
	baseDir, err := appDataDir()
	if err != nil {
		fmt.Printf("无法确定用户数据目录: %v\n", err)
		os.Exit(1)
	}
	for _, d := range []string{
		baseDir,
		filepath.Join(baseDir, "html"),
		filepath.Join(baseDir, "config"),
		filepath.Join(baseDir, "logs"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			fmt.Printf("创建目录 %s 失败: %v\n", d, err)
			os.Exit(1)
		}
	}

	// windowsgui 模式没有控制台，所有输出改写到用户数据目录的 logs/xpilot.log。
	// 必须在迁移/释放之前重定向，否则启动横幅与迁移结果写不进日志。
	if f, err := os.OpenFile(filepath.Join(baseDir, "logs", "xpilot.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		os.Stdout = f
		defer f.Close()
	}
	fmt.Printf("运行数据目录: %s\n", baseDir)

	migrateLegacyFiles(exeDir, baseDir)
	releaseEmbedded(baseDir)

	// geo 资产缺失时主实例起不来，启动时就大声说出来
	for _, name := range []string{"geoip.dat", "geosite.dat"} {
		if _, err := os.Stat(filepath.Join(baseDir, "html", name)); err != nil {
			fmt.Printf("警告: 用户数据目录 html\\ 下缺少 %s，xray 分流将无法启动；请把该文件放入 %s\\html\n", name, baseDir)
		}
	}

	svc := newService(baseDir)

	// 内嵌 xray-core 的 geoip/geosite 资产定位到用户数据目录的 html\ 文件夹
	setupXrayAssetLocation(filepath.Join(baseDir, "html"))

	svc.ensurePAC()
	// 开机/启动自愈：主端口没在跑但存在现成配置，直接用现有配置拉起
	if !portListening(fixedMainPort) {
		if err := svc.startMainFromConfig(); err == nil {
			fmt.Println("已用现有配置恢复主实例")
		}
	}

	go svc.listen()
	go svc.loops()

	select {} // 常驻
}
