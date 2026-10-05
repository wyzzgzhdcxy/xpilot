package main

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 用假数据渲染报告，验证 HTML 各关键区块都存在。
func TestReportHTML(t *testing.T) {
	runReport = reportData{started: time.Now().Add(-time.Minute), proto: "vless"}
	runReport.addSub("https://example.com/sub?token=a", 3, "")
	runReport.addSub("https://bad.example.com/sub", 0, "HTTP 503: boom")
	runReport.setFetch("vless", 5, 3, 1, 1)

	links := []string{
		"vless://11111111-2222-3333-4444-555555555555@a.com:443?security=reality&sni=x.com&fp=chrome&flow=xtls-rprx-vision#%E8%8A%82%E7%82%B9%E7%94%B2",
		"vless://11111111-2222-3333-4444-555555555555@b.com:443?security=tls&type=ws&path=%2Fws#%E8%8A%82%E7%82%B9%E4%B9%99",
		"vless://11111111-2222-3333-4444-555555555555@c.com:123#%E8%8A%82%E7%82%B9%E4%B8%99",
	}
	runReport.setNodes(links, []tcpStat{
		{addr: "a.com:443", ok: true, cost: 20 * time.Millisecond},
		{addr: "b.com:443", ok: true, cost: 35 * time.Millisecond},
		{addr: "c.com:123", ok: false, err: errors.New("refused")},
	})
	runReport.setXray([]nodeResult{
		{link: links[1], name: "节点乙", addr: "b.com:443", latencyMs: 210, speedBps: 5.2e6, speedText: "5.20 MB/s", candidate: true},
		{link: links[0], name: "节点甲", addr: "a.com:443", latencyMs: 88, speedBps: 12.9e6, speedText: "12.90 MB/s", candidate: true},
	})
	runReport.setWinner(links[0])

	dir := t.TempDir()
	runReport.writeHTML(dir)
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
	setRegistryPAC(defaultPacMain + "?stale")
	got := readPacURL()
	if !strings.HasPrefix(got, defaultPacMain+"?") || strings.Contains(got, "stale") {
		t.Fatalf("AutoConfigURL 读回 %q，应为主 PAC 地址 + 新随机参数", got)
	}
	setRegistryPAC(defaultPacMain)
	got = readPacURL()
	if !strings.HasPrefix(got, defaultPacMain+"?") {
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
		{"2026/09/20 22:45:12.123456", "127.0.0.1:5000", "tcp:github.com:443", "proxy"},
		{"2026/09/20 22:45:13.000001", "127.0.0.1:5001", "tcp:baidu.com:443", "direct"},
		{"2026/09/20 22:45:14.999999", "127.0.0.1:5002", "udp:8.8.8.8:53", "block"},
		{"2026/09/20 22:45:15.000000", "127.0.0.1:5003", "tcp:evil.com:80", "rejected"},
	}
	for i, w := range want {
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

	migrateLegacyFiles(exeDir, baseDir)

	for name, data := range map[string]string{
		"html/geoip.dat": "GEO", "html/geosite.dat": "SITE",
		"config/xpilot.json": "{}", "html/pac": "PAC", "logs/xray-access.log": "LOG",
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
	releaseEmbedded(baseDir)
	if _, err := os.Stat(filepath.Join(baseDir, "html", "console.html")); err != nil {
		t.Errorf("console.html 应被释放: %v", err)
	}
	got, _ = os.ReadFile(filepath.Join(baseDir, "html", "pac"))
	if string(got) != "PAC" {
		t.Errorf("已存在的 pac 不应被释放模板覆盖，内容 %q", got)
	}

	// 空目录首次运行：全部资源释放（console.html、pac、geo 资产）
	fresh := t.TempDir()
	releaseEmbedded(fresh)
	for _, name := range []string{"html/console.html", "html/pac", "html/geoip.dat", "html/geosite.dat"} {
		if _, err := os.Stat(filepath.Join(fresh, filepath.FromSlash(name))); err != nil {
			t.Errorf("首次运行应释放 %s: %v", name, err)
		}
	}
}
