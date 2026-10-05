package main

// 运行报告：收集一次执行的全过程信息（订阅、节点、TCP、xray 实测、最终选择），
// 渲染成单文件 HTML（内联样式与条形图，无外部依赖，离线可看），
// 每次运行覆盖写到 -report-dir 指定的目录，供浏览器直接查看。

import (
	"fmt"
	"html"
	"math"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type reportSub struct {
	url   string
	ok    bool
	count int
	err   string
}

type reportNode struct {
	link       string
	name       string
	addr       string
	tcpOK      bool
	tcpText    string
	xrayTested bool
	latencyMs  float64 // xrayTested 且验证失败时为 +Inf
	speedBps   float64
	speedText  string
	errText    string
	candidate  bool // 进入下载实测的前 top 名
	winner     bool
}

type reportData struct {
	started     time.Time
	proto       string
	subs        []reportSub
	fetchFailed int
	totalRaw    int
	deduped     int
	unparsed    int
	tcpPassed   int
	nodes       []*reportNode
	winner      string
	finished    time.Time
}

var runReport reportData

func (r *reportData) addSub(u string, count int, errMsg string) {
	r.subs = append(r.subs, reportSub{url: u, ok: errMsg == "", count: count, err: errMsg})
}

func (r *reportData) setFetch(proto string, totalRaw, deduped, unparsed, fetchFailed int) {
	r.proto, r.totalRaw, r.deduped, r.unparsed, r.fetchFailed = proto, totalRaw, deduped, unparsed, fetchFailed
}

// setNodes 依据 TCP 连通测试结果建立节点明细（顺序与去重后的节点一致；
// stats 为 nil 表示跳过了 TCP 测试，全部视为通过）。
func (r *reportData) setNodes(links []string, stats []tcpStat) {
	r.nodes = make([]*reportNode, 0, len(links))
	for i, link := range links {
		n := &reportNode{link: link, name: nodeName(link), tcpOK: true, tcpText: "未测试", latencyMs: math.Inf(1)}
		if h, p, ok := extractAddrPort(link); ok {
			n.addr = net.JoinHostPort(h, p)
		}
		if i < len(stats) {
			s := stats[i]
			n.tcpOK = s.ok
			if s.ok {
				n.tcpText = fmt.Sprintf("%.0fms", s.cost.Seconds()*1000)
			} else {
				n.tcpText = "失败"
			}
		}
		r.nodes = append(r.nodes, n)
	}
	r.tcpPassed = 0
	for _, n := range r.nodes {
		if n.tcpOK {
			r.tcpPassed++
		}
	}
}

// setXray 把 xray 实测结果合并进节点明细（candidate 标记随行携带）。
func (r *reportData) setXray(rows []nodeResult) {
	byLink := make(map[string]*reportNode, len(r.nodes))
	for _, n := range r.nodes {
		byLink[n.link] = n
	}
	for _, row := range rows {
		n, ok := byLink[row.link]
		if !ok {
			continue
		}
		n.xrayTested = true
		n.latencyMs = row.latencyMs
		n.speedBps = row.speedBps
		n.speedText = row.speedText
		n.errText = row.errText
		n.candidate = row.candidate
	}
}

func (r *reportData) setWinner(link string) {
	r.winner = link
	for _, n := range r.nodes {
		n.winner = n.link == link
	}
}

func (r *reportData) writeHTML(dir string) {
	r.finished = time.Now()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Printf("创建报告目录失败: %v\n", err)
		return
	}
	path := filepath.Join(dir, "xpilot-report.html")
	if err := os.WriteFile(path, []byte(r.html()), 0o644); err != nil {
		fmt.Printf("写出 HTML 报告失败: %v\n", err)
		return
	}
	fmt.Printf("运行报告已保存: %s\n", path)
}

func latColor(ms float64) string {
	switch {
	case ms < 150:
		return "#16a34a"
	case ms < 300:
		return "#d97706"
	default:
		return "#dc2626"
	}
}

const reportCSS = `
*{box-sizing:border-box}
body{margin:0;background:#f4f6fa;color:#1f2937;font:15px/1.6 "Segoe UI","Microsoft YaHei",system-ui,sans-serif}
.wrap{max-width:1100px;margin:0 auto;padding:28px 20px 60px}
header{background:linear-gradient(135deg,#1e3a8a,#2563eb 55%,#3b82f6);color:#fff;border-radius:16px;padding:26px 32px;margin-bottom:22px;box-shadow:0 8px 24px rgba(37,99,235,.22)}
header h1{margin:0 0 6px;font-size:24px;letter-spacing:.5px}
header .meta{color:#dbeafe;font-size:14px}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:14px;margin-bottom:22px}
.card{background:#fff;border-radius:14px;padding:16px 18px;box-shadow:0 2px 8px rgba(15,23,42,.06)}
.card .num{font-size:26px;font-weight:700;color:#1e3a8a}
.card .lbl{color:#6b7280;font-size:13px;margin-top:2px}
section{background:#fff;border-radius:14px;padding:20px 24px;margin-bottom:22px;box-shadow:0 2px 8px rgba(15,23,42,.06)}
section h2{margin:0 0 14px;font-size:17px;border-left:4px solid #2563eb;padding-left:10px}
section.final{background:linear-gradient(135deg,#ecfdf5,#d1fae5);border:1px solid #a7f3d0}
section.final h2{border-color:#10b981;color:#065f46;margin-bottom:8px}
.final-name{font-size:22px;font-weight:700;color:#065f46;margin-bottom:6px;word-break:break-all}
.final-meta{color:#047857;font-size:14px}
table{width:100%;border-collapse:collapse;font-size:14px}
th{color:#6b7280;text-align:left;font-weight:600;padding:8px 10px;border-bottom:2px solid #e5e7eb;white-space:nowrap}
td{padding:8px 10px;border-bottom:1px solid #f1f5f9;vertical-align:middle}
tr:last-child td{border-bottom:none}
tr.winner{background:#ecfdf5}
.badge{display:inline-block;padding:2px 10px;border-radius:999px;font-size:12px;font-weight:600;white-space:nowrap}
.b-ok{background:#dcfce7;color:#15803d}
.b-fail{background:#fee2e2;color:#b91c1c}
.b-cand{background:#dbeafe;color:#1d4ed8}
.b-final{background:#16a34a;color:#fff}
.b-gray{background:#e5e7eb;color:#4b5563}
.bar{height:7px;border-radius:999px;background:#eef2f7;overflow:hidden;margin-top:4px;min-width:60px}
.bar i{display:block;height:100%;border-radius:999px}
.name{max-width:280px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.addr{color:#6b7280;font-size:13px;white-space:nowrap}
.note{color:#6b7280;font-size:13px;margin:12px 0 0}
.chart .row{display:flex;align-items:center;gap:10px;margin:7px 0}
.chart .lbl{width:240px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:13px;color:#374151;text-align:right;flex:none}
.chart .track{flex:1;background:#eef2f7;border-radius:999px;height:16px;overflow:hidden}
.chart .track i{display:block;height:100%;border-radius:999px;min-width:4px}
.chart .val{width:90px;font-size:13px;color:#374151;flex:none}
footer{color:#9ca3af;font-size:13px;text-align:center;margin-top:8px}
code{background:#f1f5f9;padding:1px 6px;border-radius:6px;font-size:13px}
`

func (r *reportData) html() string {
	var b strings.Builder
	w := func(format string, a ...interface{}) { fmt.Fprintf(&b, format, a...) }
	esc := html.EscapeString

	// 预计算图表数据
	var verified []*reportNode
	maxLat, maxSpd := 1.0, 1.0
	for _, n := range r.nodes {
		if n.xrayTested && !math.IsInf(n.latencyMs, 1) {
			verified = append(verified, n)
			if n.latencyMs > maxLat {
				maxLat = n.latencyMs
			}
		}
		if n.speedBps > maxSpd {
			maxSpd = n.speedBps
		}
	}

	w(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>xpilot 运行报告 %s</title>
<style>`, r.started.Format("2006-01-02 15:04"))
	b.WriteString(reportCSS)
	w(`</style>
</head>
<body><div class="wrap">
`)

	w(`<header><h1>xpilot 运行报告</h1><div class="meta">开始时间 %s ｜ 总耗时 %s ｜ 协议过滤 %s</div></header>`,
		esc(r.started.Format("2006-01-02 15:04:05")),
		r.finished.Sub(r.started).Round(time.Second),
		esc(r.proto))

	// 最终节点
	var win *reportNode
	verifiedCount := 0
	for _, n := range r.nodes {
		if n.winner {
			win = n
		}
		if n.xrayTested && !math.IsInf(n.latencyMs, 1) {
			verifiedCount++
		}
	}
	if win != nil {
		wspeed := win.speedText
		if wspeed == "" {
			wspeed = "未测出"
		}
		w(`<section class="final"><h2>最终节点</h2><div class="final-name">%s</div>
<div class="final-meta">服务器 <code>%s</code> ｜ 延迟 %.0f ms ｜ 下载速度 %s</div></section>`,
			esc(win.name), esc(win.addr), win.latencyMs, wspeed)
	}

	// 统计卡片
	subsOK := 0
	for _, s := range r.subs {
		if s.ok {
			subsOK++
		}
	}
	w(`<div class="cards">`)
	w(`<div class="card"><div class="num">%d/%d</div><div class="lbl">订阅源成功</div></div>`, subsOK, len(r.subs))
	w(`<div class="card"><div class="num">%d</div><div class="lbl">原始节点</div></div>`, r.totalRaw)
	w(`<div class="card"><div class="num">%d</div><div class="lbl">去重后</div></div>`, r.deduped)
	w(`<div class="card"><div class="num">%d</div><div class="lbl">TCP 连通</div></div>`, r.tcpPassed)
	w(`<div class="card"><div class="num">%d</div><div class="lbl">xray 验证通过</div></div>`, verifiedCount)
	w(`</div>`)

	// 订阅源
	if len(r.subs) > 0 {
		w(`<section><h2>订阅源</h2><table><tr><th>订阅地址</th><th>状态</th><th>获取节点数</th><th>错误信息</th></tr>`)
		for _, s := range r.subs {
			status, errCell := `<span class="badge b-ok">成功</span>`, "-"
			if !s.ok {
				status, errCell = `<span class="badge b-fail">失败</span>`, esc(s.err)
			}
			w(`<tr><td class="name" title="%s">%s</td><td>%s</td><td>%d</td><td class="addr">%s</td></tr>`,
				esc(s.url), esc(s.url), status, s.count, errCell)
		}
		w(`</table><p class="note">原始节点 %d 个，无法解析地址 %d 个，按 服务器:端口 去重后 %d 个，订阅失败 %d 个。</p></section>`,
			r.totalRaw, r.unparsed, r.deduped, r.fetchFailed)
	}

	// 延迟对比图
	if len(verified) > 0 {
		lat := make([]*reportNode, len(verified))
		copy(lat, verified)
		sort.Slice(lat, func(i, j int) bool { return lat[i].latencyMs < lat[j].latencyMs })
		w(`<section><h2>延迟对比（验证通过的节点，越短越好）</h2><div class="chart">`)
		for _, n := range lat {
			pct := 100 * n.latencyMs / maxLat
			w(`<div class="row"><div class="lbl" title="%[1]s">%[1]s</div><div class="track"><i style="width:%.1f%%;background:%s"></i></div><div class="val">%.0f ms</div></div>`,
				esc(n.name), pct, latColor(n.latencyMs), n.latencyMs)
		}
		w(`</div></section>`)
	}

	// 下载速度对比图
	var spd []*reportNode
	for _, n := range r.nodes {
		if n.speedBps > 0 {
			spd = append(spd, n)
		}
	}
	if len(spd) > 0 {
		sort.Slice(spd, func(i, j int) bool { return spd[i].speedBps > spd[j].speedBps })
		w(`<section><h2>下载速度对比（实测候选节点）</h2><div class="chart">`)
		for _, n := range spd {
			pct := 100 * n.speedBps / maxSpd
			color := "#3b82f6"
			if n.winner {
				color = "#16a34a"
			}
			w(`<div class="row"><div class="lbl" title="%[1]s">%[1]s</div><div class="track"><i style="width:%.1f%%;background:%s"></i></div><div class="val">%s</div></div>`,
				esc(n.name), pct, color, n.speedText)
		}
		w(`</div></section>`)
	}

	// 节点明细表
	if len(r.nodes) > 0 {
		disp := make([]*reportNode, len(r.nodes))
		copy(disp, r.nodes)
		rank := func(n *reportNode) int {
			switch {
			case n.winner:
				return 0
			case n.xrayTested && !math.IsInf(n.latencyMs, 1):
				return 1
			case n.xrayTested:
				return 2
			default:
				return 3
			}
		}
		sort.SliceStable(disp, func(i, j int) bool {
			ri, rj := rank(disp[i]), rank(disp[j])
			if ri != rj {
				return ri < rj
			}
			return disp[i].latencyMs < disp[j].latencyMs
		})

		w(`<section><h2>节点明细（共 %d 个）</h2><table><tr><th>#</th><th>节点</th><th>服务器</th><th>TCP</th><th>延迟</th><th>下载速度</th><th>状态</th></tr>`, len(disp))
		for i, n := range disp {
			var status string
			switch {
			case n.winner:
				status = `<span class="badge b-final">最终节点</span>`
			case strings.HasPrefix(n.errText, "下载失败"):
				status = fmt.Sprintf(`<span class="badge b-fail" title="%s">下载失败</span>`, esc(n.errText))
			case n.candidate:
				status = `<span class="badge b-cand">候选</span>`
			case n.xrayTested && !math.IsInf(n.latencyMs, 1):
				status = `<span class="badge b-ok">通过</span>`
			case n.xrayTested:
				status = fmt.Sprintf(`<span class="badge b-fail" title="%s">验证失败</span>`, esc(n.errText))
			default:
				status = `<span class="badge b-gray">TCP 不通</span>`
			}

			rowClass := ""
			if n.winner {
				rowClass = ` class="winner"`
			}
			addr := n.addr
			if addr == "" {
				addr = "(无法解析地址)"
			}

			latCell, latBar := "-", ""
			if n.xrayTested && !math.IsInf(n.latencyMs, 1) {
				latCell = fmt.Sprintf("%.0f ms", n.latencyMs)
				latBar = fmt.Sprintf(`<div class="bar"><i style="width:%.0f%%;background:%s"></i></div>`,
					100*n.latencyMs/maxLat, latColor(n.latencyMs))
			} else if n.xrayTested {
				latCell = "超时"
			}

			speedCell, speedBar := "-", ""
			if n.speedBps > 0 {
				speedCell = n.speedText
				color := "#3b82f6"
				if n.winner {
					color = "#16a34a"
				}
				speedBar = fmt.Sprintf(`<div class="bar"><i style="width:%.0f%%;background:%s"></i></div>`,
					100*n.speedBps/maxSpd, color)
			}

			w(`<tr%s><td>%d</td><td class="name" title="%s">%s</td><td class="addr">%s</td><td>%s</td><td>%s%s</td><td>%s%s</td><td>%s</td></tr>`,
				rowClass, i+1, esc(n.name), esc(n.name), esc(addr), n.tcpText, latCell, latBar, speedCell, speedBar, status)
		}
		w(`</table></section>`)
	}

	w(`<footer>由 xpilot 自动生成于 %s</footer>`, esc(r.finished.Format("2006-01-02 15:04:05")))
	w(`</div></body></html>`)
	return b.String()
}
