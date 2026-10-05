package app

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/common/log"
	"github.com/xtls/xray-core/common/net"
)

// 用假数据渲染报告，验证 HTML 各关键区块都存在。
func TestReportHTML(t *testing.T) {
	RunReport = reportData{Started: time.Now().Add(-time.Minute), proto: "vless"}
	RunReport.addSub("https://example.com/sub?token=a", 3, "")
	RunReport.addSub("https://bad.example.com/sub", 0, "HTTP 503: boom")
	RunReport.setFetch("vless", 5, 3, 1, 1)

	links := []string{
		"vless://11111111-2222-3333-4444-555555555555@a.com:443?security=reality&sni=x.com&fp=chrome&flow=xtls-rprx-vision#%E8%8A%82%E7%82%B9%E7%94%B2",
		"vless://11111111-2222-3333-4444-555555555555@b.com:443?security=tls&type=ws&path=%2Fws#%E8%8A%82%E7%82%B9%E4%B9%99",
		"vless://11111111-2222-3333-4444-555555555555@c.com:123#%E8%8A%82%E7%82%B9%E4%B8%99",
	}
	RunReport.setNodes(links, []tcpStat{
		{addr: "a.com:443", ok: true, cost: 20 * time.Millisecond},
		{addr: "b.com:443", ok: true, cost: 35 * time.Millisecond},
		{addr: "c.com:123", ok: false, err: errors.New("refused")},
	})
	RunReport.setXray([]nodeResult{
		{link: links[1], name: "节点乙", addr: "b.com:443", latencyMs: 210, speedBps: 5.2e6, speedText: "5.20 MB/s", candidate: true},
		{link: links[0], name: "节点甲", addr: "a.com:443", latencyMs: 88, speedBps: 12.9e6, speedText: "12.90 MB/s", candidate: true},
	})
	RunReport.setWinner(links[0])

	dir := t.TempDir()
	RunReport.writeHTML(dir)
	data, err := os.ReadFile(filepath.Join(dir, "xpilot-report.html"))
	if err != nil {
		t.Fatalf("读取报告失败: %v", err)
	}
	s := string(data)
	for _, want := range []string{"节点甲", "节点乙", "12.90 MB/s", "210 ms", "最终节点", "HTTP 503", "TCP 不通", "延迟对比", "下载速度对比"} {
		if !strings.Contains(s, want) {
			t.Errorf("报告缺少 %q", want)
		}
	}
}

// setRegistryPAC 会真实写入 HKCU 注册表——写入后读回验证（含随机参数与旧参数剥离）。
func TestRegistryPAC(t *testing.T) {
	setRegistryPAC(DefaultPacMain + "?stale")
	got := readPacURL()
	if !strings.HasPrefix(got, DefaultPacMain+"?") || strings.Contains(got, "stale") {
		t.Fatalf("AutoConfigURL 读回 %q，应为主 PAC 地址 + 新随机参数", got)
	}
	setRegistryPAC(DefaultPacMain)
	got = readPacURL()
	if !strings.HasPrefix(got, DefaultPacMain+"?") {
		t.Fatalf("AutoConfigURL 读回 %q", got)
	}
	t.Logf("注册表 PAC 读写正常，当前指向: %s", got)
}

// 用假节点跑通 xrayTestAll 全流程：内存实例创建、协议验证失败路径、实例回收。
// 不依赖外网、不碰真实订阅。
func TestXrayTestAllFakeNodes(t *testing.T) {
	links := []string{
		"vless://11111111-2222-3333-4444-555555555555@127.0.0.1:1?type=tcp&security=none#fake-dead-1",
		"vless://11111111-2222-3333-4444-555555555555@127.0.0.1:2?type=tcp&security=none#fake-dead-2",
		"ss://YWVzLTI1Ni1nY206dGVzdA==@127.0.0.1:3#not-vless", // 非 vless，应在生成配置时报错
	}
	rows, err := xrayTestAll(links, xrayTestOpts{
		serverURL:    "https://www.gstatic.com/generate_204",
		latencyURL:   "https://127.0.0.1:1/", // 立即拒连，模拟全部节点验证失败
		maxLatency:   2 * time.Second,
		dlTimeout:    5 * time.Second,
		overall:      2 * time.Minute,
		downloadSize: 1024,
		top:          2,
	})
	if err != nil {
		t.Fatalf("xrayTestAll 返回错误: %v", err)
	}
	// ss 链接在生成配置阶段就失败，不产生结果行
	if len(rows) != 2 {
		t.Fatalf("期望 2 行结果（两个假 vless），得到 %d", len(rows))
	}
	for _, r := range rows {
		t.Logf("name=%q addr=%q latency=%v speed=%q", r.name, r.addr, r.latencyMs, r.speedText)
		if !math.IsInf(r.latencyMs, 1) {
			t.Errorf("假节点 %s 不应通过协议验证", r.name)
		}
		if r.tn.inst != nil {
			t.Errorf("假节点 %s 的 xray 实例应已被回收", r.name)
		}
	}
}

// 访问日志行解析：proxy/direct 走向、rejected、无 detour、不认识的行跳过。
func TestParseAccessLines(t *testing.T) {
	lines := []string{
		"2026/09/20 22:45:12.123456 from 127.0.0.1:5000 accepted tcp:github.com:443 [mixed-in -> proxy]",
		"2026/09/20 22:45:13.000001 from 127.0.0.1:5001 accepted tcp:baidu.com:443 [mixed-in -> direct]",
		"2026/09/20 22:45:14.999999 from 127.0.0.1:5002 accepted udp:8.8.8.8:53 [mixed-in -> block]",
		"2026/09/20 22:45:15.000000 from 127.0.0.1:5003 rejected tcp:evil.com:80",
		"2026/09/20 22:45:16.000000 something totally unrelated",
		"",
	}
	rows := parseAccessLines(lines)
	if len(rows) != 4 {
		t.Fatalf("期望解析出 4 行，得到 %d: %v", len(rows), rows)
	}
	want := [][]string{
		{"2026/09/20 22:45:12.123456", "127.0.0.1:5000", "github.com:443", "proxy"},
		{"2026/09/20 22:45:13.000001", "127.0.0.1:5001", "baidu.com:443", "direct"},
		{"2026/09/20 22:45:14.999999", "127.0.0.1:5002", "8.8.8.8:53", "block"},
		{"2026/09/20 22:45:15.000000", "127.0.0.1:5003", "evil.com:80", "rejected"},
	}
	for i, w := range want {
		for j := range w {
			if rows[i][j] != w[j] {
				t.Errorf("行 %d 字段 %d = %q，期望 %q", i, j, rows[i][j], w[j])
			}
		}
	}
}

// TestSanitizeNodeLink：把字面 \uXXXX 还原成原字符。
//
// 主要场景：聊天软件/JSON 文件复制粘贴时把 & 转义成 \u0026,导致
// url.Parse 不认作 query 分隔符,buildXrayConfig 拿不到 security/pbk/sid 等参数。
func TestSanitizeNodeLink(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// 最常见：& 被转义
		{
			name: "ampersand",
			in:   "vless://uuid@h:1?security=reality" + "\u0026" + "pbk=k",
			want: "vless://uuid@h:1?security=reality&pbk=k",
		},
		// 真实 link 形态（用户的 defaultNodeLink 全字段）
		{
			name: "full link with all params escaped",
			in: "vless://5230655e-cc0a-4432-8676-e2da3429a5ec@103.212.187.40:34433?encryption=none" + "\u0026" + "flow=xtls-rprx-vision" + "\u0026" + "security=reality" + "\u0026" + "sni=www.apple.com" + "\u0026" + "fp=chrome" + "\u0026" + "pbk=xxx" + "\u0026" + "sid=a01cc704" + "\u0026" + "type=tcp#x",
			want: "vless://5230655e-cc0a-4432-8676-e2da3429a5ec@103.212.187.40:34433?encryption=none&flow=xtls-rprx-vision&security=reality&sni=www.apple.com&fp=chrome&pbk=xxx&sid=a01cc704&type=tcp#x",
		},
		// 已经是正常 & 的 link,应该原样返回
		{
			name: "already normal",
			in:   "vless://uuid@h:1?security=reality&pbk=k",
			want: "vless://uuid@h:1?security=reality&pbk=k",
		},
		// 大写 hex 也认
		{
			name: "uppercase hex",
			in:   "x?a=1" + "\u0026" + "b=2",
			want: "x?a=1&b=2",
		},
		// 非 4 位 hex,跳过不动
		{
			name: "not hex, leave as is",
			in:   "x?a=1\\u00ZZ&b=2",
			want: "x?a=1\\u00ZZ&b=2",
		},
		// 没有 \u,直接返回（不进扫描）
		{
			name: "no escape sequence",
			in:   "vless://uuid@h:1?security=reality",
			want: "vless://uuid@h:1?security=reality",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeNodeLink(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeNodeLink(%q)\n  got:  %q\n  want: %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildXrayConfigEscapedLink：模拟"用户从聊天复制 defaultNodeLink,
// & 都被转成 \u0026"的情况,buildXrayConfig 必须能正确生成 reality 配置。
//
// 这是上一版回归的核心:不修这个 bug 时,生成的 config.json 里
// streamSettings.security 是空、realitySettings 整个缺失,主实例启动后
// reality 协议不工作。
func TestBuildXrayConfigEscapedLink(t *testing.T) {
	// 用户的真实默认节点 link,所有 & 都被转成 \u0026
	escaped := "vless://5230655e-cc0a-4432-8676-e2da3429a5ec@103.212.187.40:34433?encryption=none" + "\u0026" + "flow=xtls-rprx-vision" + "\u0026" + "security=reality" + "\u0026" + "sni=www.apple.com" + "\u0026" + "fp=chrome" + "\u0026" + "pbk=mNg2hFtDuhtZC2dq3wtrLFrouV8Oc7rYdQl9wPeHNSA" + "\u0026" + "sid=a01cc704" + "\u0026" + "type=tcp#x"

	cfgJSON, err := buildXrayConfig(escaped, 10808, false, "", "")
	if err != nil {
		t.Fatalf("buildXrayConfig 返回错误: %v", err)
	}

	// 解析生成的 JSON,验证关键字段都被正确填上
	var cfgMap map[string]interface{}
	if err := json.Unmarshal(cfgJSON, &cfgMap); err != nil {
		t.Fatalf("解析配置失败: %v", err)
	}
	outbounds, _ := cfgMap["outbounds"].([]interface{})
	if len(outbounds) == 0 {
		t.Fatal("配置里没有 outbounds")
	}
	proxy, _ := outbounds[0].(map[string]interface{})
	if proxy["protocol"] != "vless" {
		t.Errorf("出站协议应为 vless,得到 %v", proxy["protocol"])
	}
	stream, _ := proxy["streamSettings"].(map[string]interface{})
	if stream["security"] != "reality" {
		t.Errorf("security 应为 reality,得到 %v（这是 bug 主因）", stream["security"])
	}
	rs, ok := stream["realitySettings"].(map[string]interface{})
	if !ok {
		t.Fatalf("缺少 realitySettings 块（这是 bug 主因之一）")
	}
	if rs["publicKey"] != "mNg2hFtDuhtZC2dq3wtrLFrouV8Oc7rYdQl9wPeHNSA" {
		t.Errorf("publicKey 错误,得到 %v", rs["publicKey"])
	}
	if rs["serverName"] != "www.apple.com" {
		t.Errorf("serverName 错误,得到 %v", rs["serverName"])
	}
	if rs["shortId"] != "a01cc704" {
		t.Errorf("shortId 错误,得到 %v", rs["shortId"])
	}
	if rs["fingerprint"] != "chrome" {
		t.Errorf("fingerprint 错误,得到 %v", rs["fingerprint"])
	}
}

// TestParseAccessLinesRealFormat 盯的是**本机日志的实际形态**，三处都曾是页面上看得见的 bug：
//
// ① 走向列：实际日志用 `>>` 分隔（不是 `->`），早期只识别 `->`，
//	于是整串 "mixed-in >> proxy" 原样进了徽标，把右侧列撑成两行。
//
// ② 目标列：开启嗅探后网络类型位置留空，目标写成 `//collector.github.com:443`，
//	早期直接透传，页面上就出现了突兀的 `//` 开头。
//
// ③ 来源/目标列：日志里几乎都带 `tcp:` 前缀，冗长且无排查价值，把列挤爆导致
//	排版重叠；目标列更是同列里一半带 `tcp:` 一半不带，看着就像 bug。故统一剥掉。
func TestParseAccessLinesRealFormat(t *testing.T) {
	lines := []string{
		// 真实抓取：入站 mixed-in + 嗅探出域名（目标带 // 前缀）
		"2026/09/20 14:45:29.187247 from 127.0.0.1:13475 accepted //github.githubassets.com:443 [mixed-in >> proxy]",
		// 真实抓取：来源与目标都带 tcp: 前缀
		"2026/09/20 14:20:08.349513 from tcp:127.0.0.1:3005 accepted tcp:www.google.com:443 [mixed-in >> proxy]",
		"2026/09/20 14:45:29.473399 from 127.0.0.1:14953 accepted //api.github.com:443 [mixed-in >> direct]",
	}

	rows := parseAccessLines(lines)
	if len(rows) != 3 {
		t.Fatalf("期望 3 行，得到 %d: %v", len(rows), rows)
	}
	want := [][]string{
		{"2026/09/20 14:45:29.187247", "127.0.0.1:13475", "github.githubassets.com:443", "proxy"},
		{"2026/09/20 14:20:08.349513", "127.0.0.1:3005", "www.google.com:443", "proxy"},
		{"2026/09/20 14:45:29.473399", "127.0.0.1:14953", "api.github.com:443", "direct"},
	}
	for i, w := range want {
		if len(rows[i]) != len(w) {
			t.Fatalf("行 %d 字段数 %d，期望 %d", i, len(rows[i]), len(w))
		}
		for j := range w {
			if rows[i][j] != w[j] {
				t.Errorf("行 %d 字段 %d = %q，期望 %q", i, j, rows[i][j], w[j])
			}
		}
	}
}

// tailFile：正常读全部、超限截尾并丢弃半行、文件不存在报错。
func TestTailFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")

	// 文件小于上限：原样返回
	content := "line1\nline2\nline3\n"
	os.WriteFile(path, []byte(content), 0o644)
	lines, truncated, err := tailFile(path, 1024)
	if err != nil || truncated {
		t.Fatalf("小文件不应截断: err=%v truncated=%v", err, truncated)
	}
	if len(lines) != 3 || lines[0] != "line1" {
		t.Fatalf("期望 3 行，得到 %v", lines)
	}

	// 超限：只保留末尾，且半行被丢弃
	os.WriteFile(path, []byte("truncat"+"ed-half\n"+content), 0o644)
	lines, truncated, err = tailFile(path, 16)
	if err != nil || !truncated {
		t.Fatalf("超限文件应标记截断: err=%v truncated=%v", err, truncated)
	}
	for _, l := range lines {
		if l == "truncated-half" || strings.HasSuffix(l, "truncat") {
			t.Errorf("半行不应出现在结果里: %q", lines)
			break
		}
	}
	if lines[len(lines)-1] != "line3" {
		t.Errorf("末行应为 line3，得到 %q", lines)
	}

	// 不存在
	if _, _, err := tailFile(filepath.Join(dir, "none.log"), 1024); !os.IsNotExist(err) {
		t.Errorf("文件不存在应返回 IsNotExist，得到 %v", err)
	}
}


func pad2(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// appendHistory：只保留最近 HistoryLimit 条，且最新在末尾。
func TestAppendHistoryLimit(t *testing.T) {
	s := &Service{}
	for i := 0; i < HistoryLimit+30; i++ {
		s.appendHistory(runInfo{Time: time.Unix(int64(i), 0), Winner: "node" + pad6(i)})
	}
	if len(s.history) != HistoryLimit {
		t.Fatalf("期望保留 %d 条，得到 %d", HistoryLimit, len(s.history))
	}
	// 末尾应是最新那条（序号 HistoryLimit+30-1）
	want := "node" + pad6(HistoryLimit+29)
	if s.history[len(s.history)-1].Winner != want {
		t.Errorf("末条应为最新 %q，得到 %q", want, s.history[len(s.history)-1].Winner)
	}
	// 首条应是窗口起点（总数 - HistoryLimit）
	wantFirst := "node" + pad6(30)
	if s.history[0].Winner != wantFirst {
		t.Errorf("首条应为 %q，得到 %q", wantFirst, s.history[0].Winner)
	}
}

// appendSelectLog：每条记录写一行 JSON 追加到 logs/xpilot-select.log，
// 只追加不裁剪；内存 history 有上限但这个文件没有。
func TestAppendSelectLog(t *testing.T) {
	LogDir := t.TempDir()
	s := &Service{LogDir: LogDir}

	write := func(i int) runInfo {
		info := runInfo{Time: time.Unix(int64(i), 0), Winner: "node" + pad6(i), DurationS: 1.5}
		if i%3 == 0 {
			info.Error = "boom"
		} else {
			info.OK = true
			info.Metrics = &metrics{Name: "n" + pad6(i), Addr: "a.com:443", LatencyMs: int64(i)}
		}
		return info
	}
	total := HistoryLimit + 20
	for i := 0; i < total; i++ {
		s.appendSelectLog(write(i))
	}

	path := filepath.Join(LogDir, "xpilot-select.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("日志文件应存在: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	// ★ 关键：文件不受 HistoryLimit 限制，写入多少就有多少
	if len(lines) != total {
		t.Fatalf("期望 %d 行（不受内存上限约束），得到 %d", total, len(lines))
	}

	// 每行都应是合法 JSON，且字段完整
	for i, ln := range lines {
		var got runInfo
		if err := json.Unmarshal([]byte(ln), &got); err != nil {
			t.Fatalf("第 %d 行不是合法 JSON: %v (%q)", i, err, ln)
		}
		if got.Winner != "node"+pad6(i) {
			t.Errorf("第 %d 行节点应为 node%s，得到 %q", i, pad6(i), got.Winner)
		}
		if i%3 == 0 && got.Error != "boom" {
			t.Errorf("第 %d 行应带 error", i)
		}
		if i%3 != 0 && (got.Metrics == nil || got.Metrics.LatencyMs != int64(i)) {
			t.Errorf("第 %d 行 metrics 应保留", i)
		}
	}
}

func pad6(n int) string {
	s := ""
	for i := 0; i < 6; i++ {
		s = string(rune('0'+(n%10))) + s
		n /= 10
	}
	return s
}

// 旧文件迁移 + 内嵌资源释放：各历史位置的文件都应落到用户数据目录，资源只在缺失时释放。
func TestMigrateAndRelease(t *testing.T) {
	exeDir := t.TempDir()
	baseDir := t.TempDir()

	// 旧版布局：根目录的 geo 资产、config/ 与 html/ 与 logs/ 里的文件、还有一份放在 html/ 里的错位配置
	os.MkdirAll(filepath.Join(exeDir, "config"), 0o755)
	os.MkdirAll(filepath.Join(exeDir, "html"), 0o755)
	os.MkdirAll(filepath.Join(exeDir, "logs"), 0o755)
	files := map[string]string{
		"geoip.dat":            "GEO",
		"geosite.dat":          "SITE",
		"config/xpilot.json":   "{}",
		"html/pac":             "PAC",
		"html/geoip.dat":       "GEO-HTML", // 用户曾把 dat 挪进 html/，也应被找到
		"logs/xray-access.log": "LOG",
	}
	for name, data := range files {
		os.WriteFile(filepath.Join(exeDir, filepath.FromSlash(name)), []byte(data), 0o644)
	}

	MigrateLegacyFiles(exeDir, baseDir)

	for name, data := range map[string]string{
		"html/geoip.dat": "GEO", "html/geosite.dat": "SITE",
		"config/xpilot.json": "{}", "html/pac": "PAC",
		// 旧日志名迁进数据目录后会被就地改名为 xpilot-access.log（见 MigrateLegacyFiles 末尾）
		"logs/xpilot-access.log": "LOG",
	} {
		got, err := os.ReadFile(filepath.Join(baseDir, filepath.FromSlash(name)))
		if err != nil || string(got) != data {
			t.Errorf("%s 应迁移到用户数据目录且内容一致（err=%v）", name, err)
		}
	}
	// geoip.dat 应来自根目录（html/ 里那份因目标已存在被跳过）
	got, _ := os.ReadFile(filepath.Join(baseDir, "html", "geoip.dat"))
	if string(got) != "GEO" {
		t.Errorf("geoip.dat 内容 %q，应取根目录版本 GEO", got)
	}
	if _, err := os.Stat(filepath.Join(exeDir, "geoip.dat")); !os.IsNotExist(err) {
		t.Errorf("迁移后源文件应被删除")
	}

	// 释放：console.html 与 pac 出现；已有 pac 不被覆盖
	ReleaseEmbedded(baseDir, os.DirFS(".."))
	if _, err := os.Stat(filepath.Join(baseDir, "html", "console.html")); err != nil {
		t.Errorf("console.html 应被释放: %v", err)
	}
	got, _ = os.ReadFile(filepath.Join(baseDir, "html", "pac"))
	if string(got) != "PAC" {
		t.Errorf("已存在的 pac 不应被释放模板覆盖，内容 %q", got)
	}

	// 空目录首次运行：全部资源释放（console.html、pac、geo 资产）
	fresh := t.TempDir()
	ReleaseEmbedded(fresh, os.DirFS(".."))
	for _, name := range []string{"html/console.html", "html/pac", "html/geoip.dat", "html/geosite.dat"} {
		if _, err := os.Stat(filepath.Join(fresh, filepath.FromSlash(name))); err != nil {
			t.Errorf("首次运行应释放 %s: %v", name, err)
		}
	}
}

// TestLegacyLogRename 验证旧日志名（xray-*.log）就地改名为 xpilot-*.log。
//
// 日志归属方从 xray 改为 xpilot 之后文件名也跟着变，但要满足两条：
// ① 历史内容不丢（改名而非重建）；② 目标已存在时绝不覆盖。
func TestLegacyLogRename(t *testing.T) {
	exeDir := t.TempDir()
	baseDir := t.TempDir()
	os.MkdirAll(filepath.Join(baseDir, "logs"), 0o755)

	// 场景一：只有旧名 → 应改名为新名，内容保留
	oldAccess := filepath.Join(baseDir, "logs", "xray-access.log")
	os.WriteFile(oldAccess, []byte("OLD-ACCESS-CONTENT"), 0o644)
	os.WriteFile(filepath.Join(baseDir, "logs", "xray-error.log"), []byte("OLD-ERR"), 0o644)

	MigrateLegacyFiles(exeDir, baseDir)

	newAccess := filepath.Join(baseDir, "logs", "xpilot-access.log")
	if got, err := os.ReadFile(newAccess); err != nil || string(got) != "OLD-ACCESS-CONTENT" {
		t.Errorf("旧 access 日志应改名为 xpilot-access.log 且内容保留（err=%v, got=%q）", err, got)
	}
	if _, err := os.Stat(oldAccess); !os.IsNotExist(err) {
		t.Errorf("改名后旧文件应消失")
	}
	if got, _ := os.ReadFile(filepath.Join(baseDir, "logs", "xpilot-error.log")); string(got) != "OLD-ERR" {
		t.Errorf("旧 error 日志应改名且内容保留，got=%q", got)
	}

	// 场景二：新旧名都在 → 新名必须原样保留，不被旧内容覆盖
	baseDir2 := t.TempDir()
	os.MkdirAll(filepath.Join(baseDir2, "logs"), 0o755)
	os.WriteFile(filepath.Join(baseDir2, "logs", "xray-access.log"), []byte("STALE"), 0o644)
	os.WriteFile(filepath.Join(baseDir2, "logs", "xpilot-access.log"), []byte("FRESH"), 0o644)

	MigrateLegacyFiles(exeDir, baseDir2)

	if got, _ := os.ReadFile(filepath.Join(baseDir2, "logs", "xpilot-access.log")); string(got) != "FRESH" {
		t.Errorf("目标已存在时不应被覆盖，got=%q 期望 FRESH", got)
	}
}

// TestLogSurvivesInstanceChurn 是日志接管的**回归测试**。
//
// 它复现的是本项目最隐蔽的一个故障：xray 的日志 handler 是进程级全局单例，
// 每个实例创建时会抢占它、销毁后会把留在全局的 handler 变成 inactive 吞掉日志。
// 而选点流程每次都要建一堆测试实例再销毁 —— 所以不接管的话，
// 日志会在选点那一刻永久断掉，且**代理完全正常**，极具迷惑性。
//
// 修法是 xpilot 自己接管 handler（xraylog.go）并在每处实例变动后夺回。
// 把 `reclaimXrayLogs()` 置空本测试即失败 —— 说明它是真因，不是「碰巧好了」。
func TestLogSurvivesInstanceChurn(t *testing.T) {
	dir := t.TempDir()
	if err := SetupXrayLogging(dir); err != nil {
		t.Fatalf("接管日志失败: %v", err)
	}
	defer globalLogWriter.Close()

	accessPath := filepath.Join(dir, "xpilot-access.log")

	// 先写一条，确立基线
	recordAccess("127.0.0.1:1000", "example.com:443", "proxy")
	if !waitFileContains(accessPath, "example.com", 2*time.Second) {
		t.Fatalf("接管后首条日志就没写进去")
	}

	// 起若干测试实例再逐个销毁，模拟选点对实例的反复建销
	link := "vless://11111111-2222-3333-4444-555555555555@127.0.0.1:1?type=tcp&security=none#churn"
	cfg, err := buildXrayConfig(link, 0, false, "", "")
	if err != nil {
		t.Fatalf("生成配置失败: %v", err)
	}
	for i := 0; i < 3; i++ {
		inst, err := StartXrayInstance(cfg)
		if err != nil {
			t.Fatalf("第 %d 次建实例失败: %v", i, err)
		}
		inst.Close()
	}

	// 实例折腾完之后，日志必须还能写进去 —— 被掐断时会挂在这里
	recordAccess("127.0.0.1:1001", "after-churn.com:443", "proxy")
	if !waitFileContains(accessPath, "after-churn.com", 2*time.Second) {
		data, _ := os.ReadFile(accessPath)
		t.Fatalf("实例建销后日志被掐断，文件内容:\n%s", data)
	}
}

// recordAccess 用 xray 的全局日志接口播一条 access 记录，
// 走的是和生产完全相同的路径（log.Record → 全局 handler）。
func recordAccess(from, to, detour string) {
	log.Record(&log.AccessMessage{
		From:   net.ParseAddress(from),
		To:     net.ParseAddress(to),
		Status: log.AccessAccepted,
		Detour: detour,
	})
}

// waitFileContains 轮询等待文件出现指定内容（写入是即时的，留点余量）。
func waitFileContains(path, needle string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), needle) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}


// loadSelectHistory：启动时从 logs/xpilot-select.log 反序列化恢复内存 history,
// 让界面与文件保持一致。覆盖：文件不存在、空文件、坏行跳过、超 HistoryLimit 截断。
func TestLoadSelectHistory(t *testing.T) {
	dir := t.TempDir()
	LogDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(LogDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(LogDir, "xpilot-select.log")

	// 1. 文件不存在：静默成功,history 留空
	s := &Service{LogDir: LogDir}
	s.loadSelectHistory()
	if len(s.history) != 0 {
		t.Fatalf("文件不存在时应留空,得到 %d 条", len(s.history))
	}

	// 2. 文件为空：同上
	os.WriteFile(path, []byte(""), 0o644)
	s = &Service{LogDir: LogDir}
	s.loadSelectHistory()
	if len(s.history) != 0 {
		t.Fatalf("空文件应留空,得到 %d 条", len(s.history))
	}

	// 3. 正常 N 行（N < HistoryLimit）：全部加载、按文件顺序
	var b strings.Builder
	for i := 0; i < 5; i++ {
		info := runInfo{Time: time.Unix(int64(i), 0), Winner: "node" + pad6(i), DurationS: 1.0}
		j, _ := json.Marshal(info)
		b.Write(j)
		b.WriteByte('\n')
	}
	os.WriteFile(path, []byte(b.String()), 0o644)
	s = &Service{LogDir: LogDir}
	s.loadSelectHistory()
	if len(s.history) != 5 {
		t.Fatalf("期望加载 5 条,得到 %d", len(s.history))
	}
	if s.history[0].Winner != "node000000" || s.history[4].Winner != "node000004" {
		t.Errorf("顺序错误,首=%q 末=%q", s.history[0].Winner, s.history[4].Winner)
	}

	// 4. 坏行混在中间：跳过坏行,正常行保留
	b.Reset()
	b.WriteString(`{"winner":"good-1","ok":true}` + "\n")
	b.WriteString("{this is not json}\n")
	b.WriteString(`{"winner":"good-2","ok":true}` + "\n")
	b.WriteString(`{"winner":"good-3"` + "\n")  // 半截行后跟换行符(模拟崩溃遗留)
	b.WriteString(`{"winner":"good-4","ok":true}` + "\n")
	os.WriteFile(path, []byte(b.String()), 0o644)
	s = &Service{LogDir: LogDir}
	s.loadSelectHistory()
	if len(s.history) != 3 {
		t.Fatalf("坏行应被跳过,期望 3 条,得到 %d: %+v", len(s.history), s.history)
	}
	if s.history[0].Winner != "good-1" || s.history[2].Winner != "good-4" {
		t.Errorf("坏行跳过顺序错误,得到 %+v", s.history)
	}

	// 5. 超过 HistoryLimit：只保留末尾 HistoryLimit 条
	b.Reset()
	total := HistoryLimit + 30
	for i := 0; i < total; i++ {
		info := runInfo{Winner: "n" + pad6(i)}
		j, _ := json.Marshal(info)
		b.Write(j)
		b.WriteByte('\n')
	}
	os.WriteFile(path, []byte(b.String()), 0o644)
	s = &Service{LogDir: LogDir}
	s.loadSelectHistory()
	if len(s.history) != HistoryLimit {
		t.Fatalf("期望截到 HistoryLimit=%d 条,得到 %d", HistoryLimit, len(s.history))
	}
	if s.history[0].Winner != "n"+pad6(30) {
		t.Errorf("首条应为窗口起点 n%s,得到 %q", pad6(30), s.history[0].Winner)
	}
	if s.history[HistoryLimit-1].Winner != "n"+pad6(total-1) {
		t.Errorf("末条应为最新 n%s,得到 %q", pad6(total-1), s.history[HistoryLimit-1].Winner)
	}
}





// TestFmtSpeed:fmtSpeed 把 B/s 分级成人读单位,与控制台 fmtSpeed 一致。
//
// 实际行为(科学计数 1e3=1000、1e6=1MB/s 等):
//   - B/s 档用 %.0f;0/负数不特殊处理
//   - 边界 1e3 / 1e6 / 1e9,各档位保留 1~2 位小数
func TestFmtSpeed(t *testing.T) {
	cases := []struct {
		bps  float64
		want string
	}{
		{0, "0 B/s"},
		{-100, "-100 B/s"},       // fmtSpeed 不判负,严格按数字输出
		{500, "500 B/s"},         // 500 < 1e3,走 default
		{999, "999 B/s"},         // 999 < 1e3,走 default(注意不是 1024 边界,是 1000)
		{1000, "1.0 KB/s"},       // 1e3 边界,跨入 KB/s 档,%.1f
		{1023, "1.0 KB/s"},       // 同上,>= 1e3 都进 KB/s
		{1500, "1.5 KB/s"},
		{999999, "1000.0 KB/s"},  // 999999 < 1e6,还在 KB/s 档(快饱和到带"1000.0 KB/s",但不会进 MB/s)
		{1024 * 1024, "1.05 MB/s"},     // 1MB / 1e6 = 1.048576,%.2f
		{5 * 1024 * 1024, "5.24 MB/s"}, // 5MB / 1e6 = 5.24288
		{1024 * 1024 * 1024, "1.07 GB/s"},
		{2.5 * 1024 * 1024 * 1024, "2.68 GB/s"},
	}
	for _, tc := range cases {
		got := fmtSpeed(tc.bps)
		if got != tc.want {
			t.Errorf("fmtSpeed(%v) = %q,期望 %q", tc.bps, got, tc.want)
		}
	}
}
