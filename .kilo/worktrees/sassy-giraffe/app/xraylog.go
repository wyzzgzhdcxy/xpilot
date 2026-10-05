package app

// xray-core 的日志接管层。
//
// 为什么不把日志交给 xray 自己写:它的日志 handler 是**进程级全局单例**
// (`common/log` 包里 `logHandler` 只此一份),而 `app/log.New()` 在**每个实例
// 创建时都会 `log.RegisterHandler(g)` 把全局 handler 抢走**。更糟的是
// `Instance.Handle()` 首行是 `if !g.active { return }` —— 实例 `Close()` 会把
// `active` 置 false,于是这个 handler **留在全局、但吞掉所有日志**。
//
// 而 xpilot 的选点流程会给每个候选节点各起一个测试实例、用完立即销毁,
// 于是每次选点必然掐断日志。主实例还活着、还在正常转发流量,
// 现象极具迷惑性:**代理完全正常,但 access / error 日志停在选点开始的那一刻。**
//
// 解法是自己实现 `Handler` 并注册进去,且**永不关闭**。因为 xray 只在
// `app/log.New()` 里抢占全局 handler,所以每处实例创建/销毁之后都要重新夺回。

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/xtls/xray-core/common/log"
)

// ownedLogWriter 自己接管 xray 的日志写入。
//
// 它实现 `log.Handler`,被注册为全局 handler 后**永不关闭** ——
// 只要它活着,实例销毁就无法把日志掐断。
type ownedLogWriter struct {
	mu         sync.Mutex
	accessF    *os.File
	errorF     *os.File
	accessPath string
	errorPath  string
	// buf 是一次性缓冲,避免每条日志都重新分配。
	buf []byte
}

// SetupXrayLogging 在进程启动、**任何 xray 实例创建之前**接管日志。
//
// 必须在 `core.New` 之前调用:`app/log.New()` 会注册它自己的 handler,
// 谁后注册谁生效,所以每次实例创建之后还要重新夺回一次(见 reclaimXrayLogs)。
func SetupXrayLogging(LogDir string) error {
	w := &ownedLogWriter{
		accessPath: filepath.Join(LogDir, "xpilot-access.log"),
		errorPath:  filepath.Join(LogDir, "xpilot-error.log"),
	}
	if err := w.open(); err != nil {
		return err
	}
	globalLogWriter = w
	log.RegisterHandler(w)
	return nil
}

// reclaimXrayLogs 把被 xray 实例抢走的全局 handler 夺回来。
//
// 每处实例创建 / 销毁之后都要调用。调用时机是刻意的:
// `app/log.New()` 在实例创建时抢占,`Instance.Close()` 不会归还(只是把
// 自己的 active 置 false),所以两头都要收一次。
func reclaimXrayLogs() {
	if globalLogWriter != nil {
		log.RegisterHandler(globalLogWriter)
	}
}

// globalLogWriter 是进程内唯一的接管实例,供 reclaimXrayLogs 引用。
var globalLogWriter *ownedLogWriter

func (w *ownedLogWriter) open() error {
	if err := os.MkdirAll(filepath.Dir(w.accessPath), 0o755); err != nil {
		return err
	}
	af, err := os.OpenFile(w.accessPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	ef, err := os.OpenFile(w.errorPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		af.Close()
		return err
	}
	w.accessF = af
	w.errorF = ef
	return nil
}

// Handle 实现 log.Handler。
//
// 日志是观测手段,绝不该有能力拖垮主业 —— 所以这里任何写失败都静默丢弃,
// 不 panic、不返回错误(接口本身也没有返回错误的地方)。
//
// access 日志的额外动作:解析成 [时间, 来源, 目标, 走向] 四元组后顺手 publish
// 给 accessLogHub,让控制台 SSE 端点可以实时推送,而不用每 3 秒轮询 tail。
// publish 是非阻塞、慢消费者丢,不影响 xray 主流程。
func (w *ownedLogWriter) Handle(msg log.Message) {
	switch m := msg.(type) {
	case *log.AccessMessage:
		// 时间前缀由这里加:xray 的 AccessMessage.String() 本身不带时间戳,
		// 原生的时间前缀是它写文件时加的。我们的写入点等价于那个位置。
		ts := time.Now().Format("2006/01/02 15:04:05.000000")
		body := msg.String()
		w.writeTo(w.accessF, ts, body)
		// 复刻写入文件时的"ts + space + body"形态,送给解析器。
		// body 末尾可能残留 \n,但 parseAccessLine 内部会 TrimSpace,不影响。
		if row := parseAccessLine(ts + " " + body); row != nil {
			globalAccessHub.publish(row)
		}
	case *log.GeneralMessage:
		// 只写 warning 及以上,与原生 logger 的 loglevel=warning 对齐。
		if m.Severity <= log.Severity_Warning {
			w.writeTo(w.errorF, time.Now().Format("2006/01/02 15:04:05.000000"), msg.String())
		}
	}
}

// writeTo 拼一行 "时间戳 内容\n" 写出。
func (w *ownedLogWriter) writeTo(f *os.File, ts, body string) {
	if f == nil || body == "" {
		return
	}
	body = strings.TrimRight(body, "\r\n")
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf == nil {
		w.buf = make([]byte, 0, 512)
	}
	w.buf = w.buf[:0]
	w.buf = append(w.buf, ts...)
	w.buf = append(w.buf, ' ')
	w.buf = append(w.buf, body...)
	w.buf = append(w.buf, '\n')
	_, _ = f.Write(w.buf)
}

// Close 关闭日志文件句柄。
//
// 注意:**只应在进程退出时调用,绝不能在实例销毁时调用**。
// 这个 handler 必须比所有 xray 实例活得久。
func (w *ownedLogWriter) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.accessF != nil {
		w.accessF.Close()
		w.accessF = nil
	}
	if w.errorF != nil {
		w.errorF.Close()
		w.errorF = nil
	}
	return nil
}

// logPaths 返回当前接管写入的两个文件路径,供 Service 层统一引用,
// 避免路径字符串散落各处。
func logPaths(LogDir string) (access, errorLog string) {
	return filepath.Join(LogDir, "xpilot-access.log"), filepath.Join(LogDir, "xpilot-error.log")
}

// ---------- 实时访问日志订阅(accessLogHub) ----------

// accessLogHub 把新写入的 access 行推给所有已连接的控制台页面。
//
// 独立于 pushHub:载荷不同 —— 这里推的是 [时间, 来源, 目标, 走向] 四元组,
// 复用反而要把 pushHub 改成泛型/interface{},污染选点进度的代码路径。
type accessLogHub struct {
	mu   sync.Mutex
	subs map[chan []string]struct{}
}

var globalAccessHub = &accessLogHub{subs: map[chan []string]struct{}{}}

// add 注册一个订阅者。buffer 64 容忍瞬时爆发,满了就丢行(日志是新纪录在前,
// 丢一两条下一波顶上即可;轮询时代每 3 秒 1 次本来也会丢)。
func (h *accessLogHub) add() chan []string {
	ch := make(chan []string, 64)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[ch] = struct{}{}
	return ch
}

func (h *accessLogHub) remove(ch chan []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

// publish 非阻塞投递,慢消费者直接丢。
func (h *accessLogHub) publish(row []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- row:
		default:
		}
	}
}

// ---------- 访问日志解析 + 文件尾读取 ----------

// accessRe 匹配 xray 访问日志行:
// 2026/09/20 22:45:12.123456 from 127.0.0.1:5000 accepted tcp:github.com:443 [mixed-in >> proxy]
// 尾部可能还有失败原因、email 等附加字段,不参与匹配。
var accessRe = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d+) from (\S+) (accepted|rejected) (\S+)(?: \[([^]]+)\])?`)

// stripNetType 去掉地址前的网络类型,把 `tcp:host:port` / `//host:port` 统一成 `host:port`。
//
// 日志里地址有两种写法,混在同一列非常像 bug:
//   - 带网络类型:`tcp:github.com:443`(来源列几乎总是这样)
//   - 嗅探还原域名时网络类型留空:`//collector.github.com:443`(`//` 是空类型的占位符)
//
// 页面上这两列关心的只是「哪个地址」,网络类型从端口就能看出,且列头已写明含义,
// 所以前缀一律剥掉 —— 整列形态一致,也省下十几个字符宽避免排版挤压。
//
// 判据是「第一个冒号之前不含点」:`tcp` / `udp` 这类网络类型是纯字母,
// 而 IPv6 或不带端口的裸地址不会撞上这条规则。
func stripNetType(s string) string {
	s = strings.TrimPrefix(s, "//")
	if i := strings.Index(s, ":"); i >= 0 && !strings.Contains(s[:i], ".") {
		return s[i+1:]
	}
	return s
}

// cleanDetour 从入站→出站链里取出最终出站名。
//
// xray 的方括号里是完整链路,形如 `mixed-in >> proxy`(部分版本写作 `mixed-in -> proxy`),
// 前面是入站、后面才是真正命中的出站 —— 页面只关心后者,所以取最后一个分隔符之后的部分。
// 曾经只识别 `->`,遇到本机实际在用的 `>>` 就整串透传,把徽标撑成了两行。
func cleanDetour(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, ">>"); i >= 0 {
		return strings.TrimSpace(s[i+2:])
	}
	if i := strings.LastIndex(s, "->"); i >= 0 {
		return strings.TrimSpace(s[i+2:])
	}
	return s
}

// parseAccessLine 把单行日志解析成 [时间, 来源, 目标, 走向],无法识别返回 nil。
// 与 parseAccessLines 拆开是为了让 Handle() 可以单独解析当条 access 后即时 publish。
func parseAccessLine(line string) []string {
	m := accessRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return nil
	}
	out := cleanDetour(m[5])
	if m[3] != "accepted" {
		out = m[3] // rejected 单独标出
	}
	return []string{m[1], stripNetType(m[2]), stripNetType(m[4]), out}
}

// parseAccessLines 把多行日志解析成四元组数组,无法识别的行跳过。
func parseAccessLines(lines []string) [][]string {
	rows := make([][]string, 0, len(lines))
	for _, ln := range lines {
		if r := parseAccessLine(ln); r != nil {
			rows = append(rows, r)
		}
	}
	return rows
}

// tailFile 读取文件末尾 maxBytes 范围内的完整行;truncated 表示文件更大、开头被丢弃。
func tailFile(path string, maxBytes int64) (lines []string, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	off := int64(0)
	if st.Size() > maxBytes {
		off = st.Size() - maxBytes
		truncated = true
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, truncated, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, truncated, err
	}
	lines = strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
	if truncated && len(lines) > 0 {
		lines = lines[1:] // 起始位置未必对齐行首,首行不完整,丢弃
	}
	return lines, truncated, nil
}

// 访问日志接口的返回上限:最多 logMaxLines 条,新纪录在前。
// 上限放在日志模块(而不是 Service):SSE handler 连上时的首次 dump 与
// 前端表格的截断都引这个数 —— 单一权威来源。
const (
	logTailMaxBytes = 512 * 1024
	logMaxLines     = 200
)

// dumpRecentAccessRows 返回末尾 N 条解析好的四元组,新纪录在前。文件不存在
// 或为空时返回 nil(SSE handler 用空数组推一个 history 事件)。
//
// 给 SSE handler 在客户端连上时把当前已有的日志一次性补齐,之后切到 live 推送。
func dumpRecentAccessRows(accessPath string, maxLines int) [][]string {
	lines, _, err := tailFile(accessPath, logTailMaxBytes)
	if err != nil {
		return nil
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	rows := parseAccessLines(lines)
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows
}

