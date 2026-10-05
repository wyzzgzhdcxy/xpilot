package main

// 常驻服务：Web 控制台 + PAC HTTP 服务 + 定时选点 + 自愈，全部在一个进程里。
// 内嵌 xray-core 库（不再依赖 xray.exe），exe 是单文件；参数持久化在用户数据目录的
// config/xpilot.json，网页上可查看与修改；报告输出到用户数据目录的 html/ 下，
// 由本服务的 /html/ 路径直接访问。

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type appConfig struct {
	Proto             string   `json:"proto"`
	SubscribeURLs     []string `json:"subscribeURLs"`
	DefaultNodeLink   string   `json:"defaultNodeLink"`
	Top               int      `json:"top"`
	DownloadSizeMB    int      `json:"downloadSizeMB"`
	DownloadTimeoutS  int      `json:"downloadTimeoutSec"`
	MaxLatencyMS      int      `json:"maxLatencyMs"`
	SelectIntervalMin int      `json:"selectIntervalMin"`
	ServerURL         string   `json:"serverURL"`
	LatencyURL        string   `json:"latencyURL"`
	Workers           int      `json:"workers"`
	TCPTest           bool     `json:"tcpTest"`
}

// 主/从端口写死不允许修改：PAC 文件内容与此对应，改动会导致代理脱节。
const (
	fixedMainPort  = 10808
	defaultWebPort = 6001
)

func defaultConfig() appConfig {
	return appConfig{
		Proto:             "vless",
		SubscribeURLs:     []string{},
		DefaultNodeLink:   "",
		Top:               5,
		DownloadSizeMB:    10,
		DownloadTimeoutS:  60,
		MaxLatencyMS:      1000,
		SelectIntervalMin: 60,
		ServerURL:         "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-n8.1-latest-win64-gpl-8.1.zip",
		LatencyURL:        "https://github.com/robots.txt",
		Workers:           10,
		TCPTest:           true,
	}
}

// ---------- 配置存储（xpilot.json） ----------

// loadConfig 读取 config/xpilot.json；缺失的字段用默认值补齐（文件里出现的键覆盖默认值），
// 读取后总会回写一次，保证该文件一定存在。
func loadConfig(dir string) appConfig {
	cfg := defaultConfig()
	data, err := os.ReadFile(filepath.Join(dir, "xpilot.json"))
	if err == nil && json.Unmarshal(data, &cfg) != nil {
		fmt.Println("xpilot.json 解析失败，使用默认配置")
		cfg = defaultConfig()
	}
	// 回写一次，保证 xpilot.json 一定存在
	if err := cfg.save(dir); err != nil {
		fmt.Printf("保存 xpilot.json 失败: %v\n", err)
	}
	return cfg
}

func (c appConfig) save(dir string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "xpilot.json"), data, 0o644)
}

type runInfo struct {
	Running   bool      `json:"running"`
	Time      time.Time `json:"time"`
	OK        bool      `json:"ok"`
	Error     string    `json:"error,omitempty"`
	Winner    string    `json:"winner,omitempty"`
	DurationS float64   `json:"durationSec"`
}

type service struct {
	mu       sync.Mutex
	cfg      appConfig
	running  bool
	lastRun  runInfo
	stage    string // 当前选点阶段，控制台实时显示
	mainInst *xrayInstance
	hub      pushHub // 选点进度广播（SSE）
	baseDir  string  // 用户数据目录：html/、config/、logs/ 与 geo 资产都在这里
	htmlDir  string
	logDir   string
	cfgDir   string
	pacURL   string
}

func newService(baseDir string) *service {
	s := &service{
		baseDir: baseDir,
		hub:     pushHub{subs: map[chan string]struct{}{}},
		htmlDir: filepath.Join(baseDir, "html"),
		logDir:  filepath.Join(baseDir, "logs"),
		cfgDir:  filepath.Join(baseDir, "config"),
	}
	os.MkdirAll(s.logDir, 0o755)
	os.MkdirAll(s.cfgDir, 0o755)
	s.pacURL = readPacURL()
	s.cfg = loadConfig(s.cfgDir)
	if err := os.MkdirAll(s.htmlDir, 0o755); err != nil {
		fmt.Printf("创建 html 目录失败: %v\n", err)
	}
	return s
}

// ---------- 内嵌 xray 实例管理 ----------

// startMain 在主端口（10808）上以给定节点启动主实例，已有实例先关闭。
func (s *service) startMain(link string) error {
	s.stopMain()
	if portListening(fixedMainPort) {
		// 端口仍被占用：外部残留进程（如旧版本 xray.exe），清掉
		killPortListeners(fixedMainPort)
		waitPortFree(fixedMainPort, 2*time.Second)
	}
	cfgJSON, err := buildXrayConfig(link, fixedMainPort, true, filepath.Join(s.logDir, "xray-access.log"), filepath.Join(s.logDir, "xray-error.log"))
	if err != nil {
		return err
	}
	inst, err := startXrayInstance(cfgJSON)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.mainInst = inst
	s.mu.Unlock()
	return nil
}

// stopMain 关闭主实例（释放监听端口与全部连接）。
func (s *service) stopMain() {
	s.mu.Lock()
	inst := s.mainInst
	s.mainInst = nil
	s.mu.Unlock()
	if inst != nil {
		inst.Close()
	}
}

func (s *service) mainAlive() bool {
	return portListening(fixedMainPort)
}

// startMainFromConfig 用 config/config.json 里已保存的配置启动主实例
// （开机自愈：正在用的节点参数原样恢复，不重新生成配置）。
func (s *service) startMainFromConfig() error {
	if portListening(fixedMainPort) {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(s.cfgDir, "config.json"))
	if err != nil {
		return fmt.Errorf("没有现成配置")
	}
	// 注入当前日志路径（旧版 config.json 可能没有 error 日志字段）
	var cfgMap map[string]interface{}
	if json.Unmarshal(data, &cfgMap) == nil {
		logCfg, _ := cfgMap["log"].(map[string]interface{})
		if logCfg == nil {
			logCfg = map[string]interface{}{}
			cfgMap["log"] = logCfg
		}
		logCfg["access"] = filepath.Join(s.logDir, "xray-access.log")
		logCfg["error"] = filepath.Join(s.logDir, "xray-error.log")
		data, _ = json.Marshal(cfgMap)
	}
	inst, err := startXrayInstance(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.mainInst = inst
	s.mu.Unlock()
	return nil
}

// ensurePAC 启动自检：AutoDetect 保持关闭；把 PAC 指向主端口并刷新随机串
// （被其他软件删掉时恢复，手改过的 PAC 规则也随之生效）。只在启动时做一次。
func (s *service) ensurePAC() {
	hideWin(exec.Command("reg", "add", regProxyKey, "/v", "AutoDetect", "/t", "REG_DWORD", "/d", "0", "/f")).Run()
	s.pacURL = setRegistryPAC(defaultPacMain)
}

// ---------- PAC 文件 ----------

// ---------- PAC 文件与注册表 ----------

// pacHandler 返回 html/ 目录下的 PAC 规则文件（用户自己的文件，可手动编辑，
// 改完点网页上的"刷新 PAC"或等下次运行自动刷新随机串生效）。
func (s *service) pacHandler(file string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := os.ReadFile(filepath.Join(s.htmlDir, file))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
		w.Write(data)
	}
}

// openRegedit 写入注册表编辑器的 LastKey 并启动 regedit，
// 打开后直接定位到系统代理 PAC 所在的注册表位置。
// 注意：LastKey 只在 regedit 启动时读取，regedit 已开着时需先关闭它再点。
func (s *service) openRegedit() error {
	const appletsKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Applets\Regedit`
	const lastKey = `计算机\HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	hideWin(exec.Command("reg", "add", appletsKey, "/v", "LastKey", "/t", "REG_SZ", "/d", lastKey, "/f")).Run()
	return exec.Command("regedit").Start()
}

func (s *service) handleOpenRegedit(w http.ResponseWriter, r *http.Request) {
	if err := s.openRegedit(); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// ---------- HTTP ----------

func (s *service) listen() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		// 控制台页面放在 html/console.html，每次请求都从磁盘读取，改完刷新即生效
		data, err := os.ReadFile(filepath.Join(s.htmlDir, "console.html"))
		if err != nil {
			http.Error(w, "缺少 html/console.html 控制台页面文件", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	})
	mux.HandleFunc("/pac", s.pacHandler("pac"))
	mux.HandleFunc("/api/config", s.handleConfig) // GET=页面加载时的参数与状态快照，POST=保存参数
	mux.HandleFunc("/api/events", s.handleEvents) // SSE：实时推送选点阶段与完成事件
	mux.HandleFunc("/api/run", s.handleRun)
	mux.HandleFunc("/api/default-node", s.handleDefaultNode)
	mux.HandleFunc("/api/pac-refresh", s.handlePacRefresh)
	mux.HandleFunc("/api/open-regedit", s.handleOpenRegedit)
	mux.HandleFunc("/api/access-log", s.handleAccessLog)
	mux.HandleFunc("/api/file", s.handleFileView)
	mux.Handle("/html/", http.StripPrefix("/html/", http.FileServer(http.Dir(s.htmlDir))))

	// 服务只绑定 127.0.0.1；这里再加一道来源校验兜底，非本机回环地址一律拒绝
	loopbackOnly := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	addr := fmt.Sprintf("127.0.0.1:%d", defaultWebPort)
	fmt.Printf("Web 控制台: http://127.0.0.1:%d/ ｜ PAC: %s\n", defaultWebPort, defaultPacMain)
	if err := http.ListenAndServe(addr, loopbackOnly(mux)); err != nil {
		// 端口被占用通常意味着 xpilot 已经在运行，第二个实例直接退出（兼做单实例锁）
		fmt.Printf("HTTP 服务启动失败，进程退出: %v\n", err)
		os.Exit(1)
	}
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// handleConfig：GET 返回页面加载用的参数与状态快照（不轮询，前端只在打开页面和
// 收到完成事件时各取一次）；POST 保存网页提交的参数（只覆盖提交了的字段）。
func (s *service) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		cfg, last, running, stage, pacURL := s.cfg, s.lastRun, s.running, s.stage, s.pacURL
		s.mu.Unlock()
		nextRun := "即将运行"
		if !last.Time.IsZero() {
			nextRun = last.Time.Add(time.Duration(cfg.SelectIntervalMin) * time.Minute).Format("01-02 15:04")
		}
		writeJSON(w, map[string]interface{}{
			"cfg":         cfg,
			"defaults":    defaultConfig(),
			"running":     running,
			"stage":       stage,
			"lastRun":     last,
			"pacURL":      pacURL,
			"nextRun":     nextRun,
			"currentAddr": currentNodeAddr(s.cfgDir),
		})
	case http.MethodPost:
		s.mu.Lock()
		cfg := s.cfg
		s.mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "参数格式错误: " + err.Error()})
			return
		}
		if cfg.Top < 1 || cfg.DownloadSizeMB < 1 || cfg.SelectIntervalMin < 5 {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "参数不合法（候选数≥1，下载上限≥1 MB，间隔≥5 分钟）"})
			return
		}
		s.mu.Lock()
		s.cfg = cfg
		s.mu.Unlock()
		if err := cfg.save(s.cfgDir); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleFileView 返回数据目录下指定文件的文本内容，供控制台"配置文件"页展示。
// name 只接受白名单，防止任意路径读取。
func (s *service) handleFileView(w http.ResponseWriter, r *http.Request) {
	files := map[string]string{
		"pac":    filepath.Join(s.htmlDir, "pac"),
		"xray":   filepath.Join(s.cfgDir, "config.json"),
		"xpilot": filepath.Join(s.cfgDir, "xpilot.json"),
	}
	path, ok := files[r.URL.Query().Get("name")]
	if !ok {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "未知文件"})
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "读取失败（文件可能尚未生成）: " + path})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "content": string(data), "path": path})
}

// ---------- 选点进度推送（SSE） ----------

// pushHub 向所有已连接的控制台页面广播选点阶段与完成事件。
type pushHub struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

func (h *pushHub) add() chan string {
	ch := make(chan string, 16)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[ch] = struct{}{}
	return ch
}

func (h *pushHub) remove(ch chan string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, ch)
}

// publish 非阻塞投递，慢消费者直接丢弃（阶段事件丢了也只是少一行显示）。
func (h *pushHub) publish(msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
		}
	}
}

// pushEvent 把事件序列化成 {type, text} JSON 后广播。
func (s *service) pushEvent(typ, text string) {
	data, _ := json.Marshal(map[string]string{"type": typ, "text": text})
	s.hub.publish(string(data))
}

// handleEvents SSE 长连接：建立后先推一次当前阶段（接上正在进行中的选点），
// 之后每有阶段变化/完成事件即实时下发，15 秒一次心跳防连接超时。
func (s *service) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := s.hub.add()
	defer s.hub.remove(ch)

	s.mu.Lock()
	running, stage := s.running, s.stage
	s.mu.Unlock()
	if running {
		fmt.Fprintf(w, "data: {\"type\":\"stage\",\"text\":%q}\n\n", stage)
	}
	fl.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			fl.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// startSelection 防重入地发起一次选点：同步置位运行标志，点击后状态立即可见。
func (s *service) startSelection() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.stage = "准备中"
	s.mu.Unlock()
	go s.runSelection()
}

func (s *service) handleRun(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "选点正在进行中"})
		return
	}
	s.startSelection()
	writeJSON(w, map[string]interface{}{"ok": true})
}

func (s *service) handleDefaultNode(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "选点正在进行中"})
		return
	}
	cfg := s.cfg
	if cfg.DefaultNodeLink == "" {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "未配置默认节点，请在参数中填写 defaultNodeLink"})
		return
	}
	go func() {
		s.mu.Lock()
		s.running = true
		s.mu.Unlock()
		start := time.Now()
		w := nodeResult{link: cfg.DefaultNodeLink, name: "my_xray"}
		if h, p, ok := extractAddrPort(cfg.DefaultNodeLink); ok {
			w.addr = net.JoinHostPort(h, p)
		}
		s.applyWinner(w, currentNodeAddr(s.cfgDir))
		s.mu.Lock()
		s.running = false
		s.lastRun = runInfo{Time: start, OK: true, Winner: "默认节点 my_xray", DurationS: time.Since(start).Seconds()}
		s.mu.Unlock()
		s.pushEvent("done", "已应用默认节点 my_xray")
	}()
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handlePacRefresh 重写注册表 PAC（带新随机串），让手动修改过的 PAC 规则立即生效。
func (s *service) handlePacRefresh(w http.ResponseWriter, r *http.Request) {
	s.pacURL = setRegistryPAC(defaultPacMain)
	writeJSON(w, map[string]interface{}{"ok": true})
}

// ---------- 访问日志 ----------

// accessRe 匹配 xray 访问日志行：
// 2026/09/20 22:45:12.123456 from 127.0.0.1:5000 accepted tcp:github.com:443 [mixed-in -> proxy]
// 尾部可能还有失败原因、email 等附加字段，不参与匹配。
var accessRe = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}\.\d+) from (\S+) (accepted|rejected) (\S+)(?: \[([^]]+)\])?`)

// parseAccessLines 把日志行解析成 [时间, 来源, 目标, 走向] 四元组，无法识别的行跳过。
func parseAccessLines(lines []string) [][]string {
	rows := make([][]string, 0, len(lines))
	for _, ln := range lines {
		m := accessRe.FindStringSubmatch(strings.TrimSpace(ln))
		if m == nil {
			continue
		}
		out := ""
		if detour := strings.TrimSpace(m[5]); detour != "" {
			if i := strings.LastIndex(detour, "->"); i >= 0 {
				out = strings.TrimSpace(detour[i+2:])
			} else {
				out = detour
			}
		}
		if m[3] != "accepted" {
			out = m[3] // rejected 单独标出
		}
		rows = append(rows, []string{m[1], m[2], m[4], out})
	}
	return rows
}

// tailFile 读取文件末尾 maxBytes 范围内的完整行；truncated 表示文件更大、开头被丢弃。
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
		lines = lines[1:] // 起始位置未必对齐行首，首行不完整，丢弃
	}
	return lines, truncated, nil
}

// handleAccessLog 返回 xray 访问日志末尾的解析结果：最多 400 条，新纪录在前。
func (s *service) handleAccessLog(w http.ResponseWriter, r *http.Request) {
	const maxBytes = 512 * 1024
	const maxLines = 400
	lines, truncated, err := tailFile(filepath.Join(s.logDir, "xray-access.log"), maxBytes)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, map[string]interface{}{"ok": true, "rows": [][]string{}, "truncated": false})
			return
		}
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
		truncated = true
	}
	rows := parseAccessLines(lines)
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	writeJSON(w, map[string]interface{}{"ok": true, "rows": rows, "truncated": truncated})
}

// ---------- 选点流水线与调度 ----------

// runSelection 执行一次完整选点并记录结果（通常由 startSelection 发起）。
func (s *service) runSelection() {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()

	start := time.Now()
	winner, err := s.selectBest(cfg)
	dur := time.Since(start)

	if err != nil {
		// 选点失败兜底：主实例活着就保持现状，死了用默认节点拉起
		fmt.Printf("选点失败: %v\n", err)
		s.fallbackDefaultNode()
	}

	info := runInfo{Time: start, DurationS: dur.Seconds(), Winner: winner}
	if err != nil {
		info.Error = err.Error()
	} else {
		info.OK = true
	}

	// 先落地状态再广播完成事件，保证前端收到事件后拉快照能拿到最新数据
	s.mu.Lock()
	s.running = false
	s.stage = ""
	s.lastRun = info
	s.mu.Unlock()
	if err != nil {
		s.pushEvent("done", "选点失败："+err.Error())
	} else {
		s.pushEvent("done", "选点完成："+winner)
	}
}

// setStage 更新选点阶段（供控制台显示），并实时推送给所有已连接的页面。
func (s *service) setStage(t string) {
	s.mu.Lock()
	s.stage = t
	s.mu.Unlock()
	s.pushEvent("stage", t)
}

// selectBest 完整流水线：订阅 → 过滤去重 → TCP → xray 实测 → 热替换落地 → 报告。
func (s *service) selectBest(cfg appConfig) (winner string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	if len(cfg.SubscribeURLs) == 0 {
		return "", fmt.Errorf("未配置订阅地址")
	}

	totalRaw, failed := 0, 0
	var allLinks []string
	s.setStage("拉取订阅中")
	for _, u := range cfg.SubscribeURLs {
		fmt.Printf("GET %s\n", u)
		body, err := fetchBody(u)
		if err != nil {
			fmt.Printf("  失败: %v\n", err)
			failed++
			runReport.addSub(u, 0, err.Error())
			continue
		}
		links := decodeSubscription(body)
		kept := 0
		for _, l := range links {
			if matchProto(l, cfg.Proto) {
				links[kept] = l
				kept++
			}
		}
		links = links[:kept]
		fmt.Printf("  获取到 %d 个 %s 节点\n", len(links), protoName(cfg.Proto))
		runReport.addSub(u, len(links), "")
		totalRaw += len(links)
		allLinks = append(allLinks, links...)
	}
	if totalRaw == 0 {
		return "", fmt.Errorf("没有获取到任何节点（%d 个订阅失败）", failed)
	}

	seen := make(map[string]struct{})
	deduped := make([]string, 0, len(allLinks))
	unparsed := 0
	for _, link := range allLinks {
		key := ""
		if h, p, ok := extractAddrPort(link); ok {
			key = strings.ToLower(strings.TrimSpace(h)) + ":" + p
		} else {
			unparsed++
		}
		if key != "" {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}
		deduped = append(deduped, link)
	}
	runReport.setFetch(protoName(cfg.Proto), totalRaw, len(deduped), unparsed, failed)

	tested := deduped
	if cfg.TCPTest {
		s.setStage("TCP 连通测试中")
		fmt.Printf("开始 TCP 连通测试（超时 1s，并发 %d）...\n", cfg.Workers)
		var tcpStats []tcpStat
		tested, tcpStats = testAndFilter(deduped, time.Second, cfg.Workers)
		runReport.setNodes(deduped, tcpStats)
		if len(tested) == 0 {
			return "", fmt.Errorf("所有节点均无法连通")
		}
	} else {
		runReport.setNodes(deduped, nil)
	}

	listPath := filepath.Join(s.logDir, "node.list.txt")
	if err := os.WriteFile(listPath, []byte(strings.Join(tested, "\n")+"\n"), 0o644); err != nil {
		return "", err
	}
	fmt.Printf("共 %d 个节点（%s），去重后 %d 个，连通测试通过 %d 个，无法解析地址 %d 个\n",
		totalRaw, protoName(cfg.Proto), len(deduped), len(tested), unparsed)

	curAddr := currentNodeAddr(s.cfgDir)
	rows, err := xrayTestAll(tested, xrayTestOpts{
		serverURL:    cfg.ServerURL,
		latencyURL:   cfg.LatencyURL,
		currentAddr:  curAddr,
		maxLatency:   time.Duration(cfg.MaxLatencyMS) * time.Millisecond,
		dlTimeout:    time.Duration(cfg.DownloadTimeoutS) * time.Second,
		overall:      40 * time.Minute,
		downloadSize: cfg.DownloadSizeMB * 1024 * 1024,
		top:          cfg.Top,
		onStage:      s.setStage,
	})
	if err != nil {
		return "", err
	}
	runReport.setXray(rows)
	w, ok := saveBestAndPick(rows, s.logDir, curAddr)
	if !ok {
		return "", fmt.Errorf("没有任何节点通过 xray 实测")
	}
	runReport.setWinner(w.link)
	runReport.writeHTML(s.htmlDir)
	s.applyWinner(w, curAddr)
	return fmt.Sprintf("%s（%s，%.0fms，%s）", w.name, w.addr, w.latencyMs, w.speedText), nil
}

// loops 常驻循环：健康检查（主实例掉线就用现有配置原地拉起）+ 到期触发选点。
// 计划任务只需负责保证本进程开机自启，其余不依赖它。
func (s *service) loops() {
	for {
		time.Sleep(15 * time.Second)
		s.mu.Lock()
		running, cfg := s.running, s.cfg
		s.mu.Unlock()
		if running {
			continue
		}
		// 健康检查：主实例掉线就用现有配置原地拉起
		if !s.mainAlive() {
			if err := s.startMainFromConfig(); err == nil {
				fmt.Println("看门狗：主实例掉线，已用现有配置重新拉起")
			}
		}
		// 到期选点（lastRun 为零值时视为从未运行，启动后立即执行一轮）
		s.mu.Lock()
		due := time.Since(s.lastRun.Time) >= time.Duration(cfg.SelectIntervalMin)*time.Minute
		s.mu.Unlock()
		if due {
			s.runSelection()
		}
	}
}

// applyWinner 把最优节点落地：主实例运行中 → 热替换其 proxy 出站（监听端口与
// 实例保持不动，旧出站延迟关闭让在途连接排空，浏览器零感知）；主实例未运行 →
// 直接以该节点启动主实例。落地后回写 config.json 供开机自愈与粘性判断使用。
func (s *service) applyWinner(w nodeResult, curAddr string) {
	sameNode := curAddr != "" && strings.EqualFold(w.addr, curAddr)

	if !s.mainAlive() {
		fmt.Println("主实例未在运行，直接启动")
		if err := s.startMain(w.link); err != nil {
			fmt.Printf("主实例启动失败: %v\n", err)
			return
		}
	} else {
		if sameNode {
			fmt.Println("最优节点与当前一致，无需切换")
			return
		}
		if err := s.swapMainOutbound(w.link); err != nil {
			fmt.Printf("热替换失败，回退为重启实例: %v\n", err)
			if err := s.startMain(w.link); err != nil {
				fmt.Printf("主实例启动失败: %v\n", err)
				return
			}
		}
	}

	if err := verifyOutbound(fixedMainPort); err != nil {
		fmt.Printf("警告: 出网检查失败: %v\n", err)
	}
	// 回写当前节点的启动配置（config.json 的 outbounds 已指向新节点）
	if cfgJSON, err := buildXrayConfig(w.link, fixedMainPort, true, filepath.Join(s.logDir, "xray-access.log"), filepath.Join(s.logDir, "xray-error.log")); err == nil {
		os.WriteFile(filepath.Join(s.cfgDir, "config.json"), cfgJSON, 0o644)
	}
}

// swapMainOutbound 热替换主实例的 proxy 出站（委托给 xraycore.go 的接入层）。
func (s *service) swapMainOutbound(link string) error {
	if s.mainInst == nil {
		return fmt.Errorf("主实例未运行")
	}
	cfgJSON, err := buildXrayConfig(link, fixedMainPort, true, filepath.Join(s.logDir, "xray-access.log"), filepath.Join(s.logDir, "xray-error.log"))
	if err != nil {
		return err
	}
	return s.mainInst.swapOutbound(cfgJSON)
}

// fallbackDefaultNode 选点失败时的兜底：主实例还在跑就保持现状（它此前验证过），
// 否则用内置默认节点拉起。
func (s *service) fallbackDefaultNode() {
	if s.mainAlive() {
		fmt.Printf("选点失败，但主实例已在 127.0.0.1:10808 运行，保持现状\n")
		return
	}
	fmt.Println("选点失败且主实例未运行，自动回退到内置默认节点")
	w := nodeResult{link: s.cfg.DefaultNodeLink, name: "my_xray"}
	if h, p, ok := extractAddrPort(s.cfg.DefaultNodeLink); ok {
		w.addr = net.JoinHostPort(h, p)
	}
	s.applyWinner(w, currentNodeAddr(s.cfgDir))
}
