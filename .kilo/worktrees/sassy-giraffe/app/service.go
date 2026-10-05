package app

// 常驻服务：Web 控制台 + PAC HTTP 服务 + 定时选点 + 自愈，全部在一个进程里。
// 内嵌 xray-core 库（不再依赖 xray.exe），exe 是单文件；参数持久化在用户数据目录的
// config/xpilot.json，网页上可查看与修改；报告输出到用户数据目录的 html/ 下，
// 由本服务的 /html/ 路径直接访问。

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	// 体检调度：每 SelectIntervalMin 分钟到期先体检当前节点，达标则跳过本轮选点。
	// 决策分两级：
	//   快检（fast）：延迟<ProbeLatencyFastMs 且 丢包率<ProbeLossFast → 直接跳过（不测速）
	//   全检（full）：延迟<ProbeLatencyFullMs 且 丢包率≤ProbeLossFull → 测速，≥ProbeMinSpeedBps 跳过
	//   任一不达标：走完整选点
	ProbeLatencyFastMs  int     `json:"probeLatencyFastMs"`   // 快检延迟阈值，默认 200
	ProbeLossFast       float64 `json:"probeLossFast"`        // 快检丢包阈值 0~1，默认 0.01
	ProbeLatencyFullMs  int     `json:"probeLatencyFullMs"`   // 全检延迟阈值，默认 400
	ProbeLossFull       float64 `json:"probeLossFull"`        // 全检丢包阈值 0~1，默认 0.05
	ProbeMinSpeedBps    float64 `json:"probeMinSpeedBps"`     // 全检最低下载速度（B/s），默认 5*1024*1024
}

// 主/从端口写死不允许修改：PAC 文件内容与此对应，改动会导致代理脱节。
const (
	FixedMainPort  = 10808
	DefaultWebPort = 6001
)

func defaultConfig() appConfig {
	return appConfig{
		Proto:               "vless",
		SubscribeURLs:       []string{},
		DefaultNodeLink:     "",
		Top:                 5,
		DownloadSizeMB:      10,
		DownloadTimeoutS:    60,
		MaxLatencyMS:        1000,
		SelectIntervalMin:   60,
		ServerURL:           "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-n8.1-latest-win64-gpl-8.1.zip",
		LatencyURL:          "https://github.com/robots.txt",
		Workers:             10,
		TCPTest:             true,
		ProbeLatencyFastMs:  200,
		ProbeLossFast:       0.01,
		ProbeLatencyFullMs:  400,
		ProbeLossFull:       0.05,
		ProbeMinSpeedBps:    5 * 1024 * 1024,
	}
}

// ---------- 配置存储（xpilot.json） ----------

// 参数的合法范围（唯一权威来源）。前端 console.html 的 FIELDS 里有一份等价定义，
// 两边改动时必须同步——前端负责即时提示，这里负责最终把关。
const (
	cfgTopMin, cfgTopMax                             = 1, 50
	cfgDownloadSizeMBMin, cfgDownloadSizeMBMax       = 1, 1024
	cfgDownloadTimeoutSMin, cfgDownloadTimeoutSMax   = 5, 1800
	cfgMaxLatencyMSMin, cfgMaxLatencyMSMax           = 100, 60000
	cfgSelectIntervalMinMin, cfgSelectIntervalMinMax = 5, 1440
	cfgWorkersMin, cfgWorkersMax                     = 1, 200
)

// validateConfig 校验参数取值范围，返回空字符串表示通过，否则返回给用户看的错误说明。
// 重点防两类会真正破坏运行的值：
//   - Workers：0 会让 testAndFilter 的 make(chan struct{}, 0) 永久阻塞（选点死锁），
//     负数会让 make 直接 panic
//   - 各超时：0 会让所有节点瞬间判定失败，选点永远选不出节点
func validateConfig(c appConfig) string {
	type rule struct {
		name     string
		val      int
		min, max int
	}
	for _, r := range []rule{
		{"候选数", c.Top, cfgTopMin, cfgTopMax},
		{"单节点下载上限", c.DownloadSizeMB, cfgDownloadSizeMBMin, cfgDownloadSizeMBMax},
		{"单节点下载超时", c.DownloadTimeoutS, cfgDownloadTimeoutSMin, cfgDownloadTimeoutSMax},
		{"延迟验证超时", c.MaxLatencyMS, cfgMaxLatencyMSMin, cfgMaxLatencyMSMax},
		{"自动选点间隔", c.SelectIntervalMin, cfgSelectIntervalMinMin, cfgSelectIntervalMinMax},
		{"TCP 测试并发数", c.Workers, cfgWorkersMin, cfgWorkersMax},
	} {
		if r.val < r.min || r.val > r.max {
			return fmt.Sprintf("%s不合法：%d 超出允许范围 %d ~ %d", r.name, r.val, r.min, r.max)
		}
	}
	return ""
}

// loadConfig 读取 config/xpilot.json；缺失的字段用默认值补齐（文件里出现的键覆盖默认值），
// 读取后总会回写一次，保证该文件一定存在。
// 注意：这里不调用 validateConfig 拦启动——配置文件被改坏时，宁可带着可疑参数跑起来
// 让用户能在网页上看到并改回，也不要因为一个参数不合法就拒绝启动（那样用户只能手改文件）。
// 非法值会在下一次保存时被后端拒绝，控制台的"参数设置"页也会显示出实际值。
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
	Metrics   *metrics  `json:"metrics,omitempty"`
	// Decision 标记这条记录是怎么结束的:
	//   ""         完整选点
	//   "fast"     体检快检通过(延迟+丢包达标,不测速)
	//   "full"     体检全检通过(延迟+丢包+速度达标)
	//   "fail"     体检失败(xray 不通/超时等)
	//   "trigger" 体检未达标,触发了完整选点(此条是体检记录;完整选点会再写一条)
	Decision string `json:"decision,omitempty"`
}

// metrics 是选中节点的结构化指标。
//
// 它和 Winner 字符串刻意并存：Winner 给人看（含节点名与地址的整句话），
// metrics 给程序算（画趋势图、按阈值着色）。只留 Winner 会让前端只能靠
// 解析字符串来拿延迟，改一次文案就崩。
type metrics struct {
	Name       string  `json:"name"`
	Addr       string  `json:"addr"`
	LatencyMs  int64   `json:"latencyMs"`
	PacketLoss float64 `json:"packetLoss"` // 0~1（例 0.33 = 33%），验证失败时为 1
	SpeedBps   float64 `json:"speedBps"`
	SpeedText  string  `json:"speedText"`
}

// HistoryLimit 是选点历史在内存里保留的条数上限（界面「选点详情」最多显示这么多条）。
// 超出后从最旧的开始丢弃。
//
// 注意：内存这份只是「给界面看的窗口」，但启动时会从 logs/xpilot-select.log
// 反序列化灌入（见 loadSelectHistory），所以重启后界面与文件保持一致 ——
// 文件里最近 100 条以外的旧记录不再可见。
// （2026-09-20 变更：此前要求纯内存不落盘，现已改为同时落盘并启动时加载。）
const HistoryLimit = 100

type Service struct {
	mu      sync.Mutex
	cfg     appConfig
	running bool
	lastRun runInfo
	// history 是追加式选点历史，最新在后。
	// 内存里只保留最近 HistoryLimit 条（界面展示用）；完整记录另存 logs/xpilot-select.log。
	history []runInfo
	stage    string // 当前选点阶段，控制台实时显示
	mainInst *XrayInstance
	hub      pushHub // 选点进度广播（SSE）
	baseDir  string  // 用户数据目录：html/、config/、logs/ 与 geo 资产都在这里
	htmlDir  string
	LogDir   string
	cfgDir   string
	pacURL   string
}

func NewService(baseDir string) *Service {
	s := &Service{
		baseDir: baseDir,
		hub:     pushHub{subs: map[chan string]struct{}{}},
		htmlDir: filepath.Join(baseDir, "html"),
		LogDir:  filepath.Join(baseDir, "logs"),
		cfgDir:  filepath.Join(baseDir, "config"),
	}
	os.MkdirAll(s.LogDir, 0o755)
	os.MkdirAll(s.cfgDir, 0o755)
	s.pacURL = readPacURL()
	s.cfg = loadConfig(s.cfgDir)
	if err := os.MkdirAll(s.htmlDir, 0o755); err != nil {
		fmt.Printf("创建 html 目录失败: %v\n", err)
	}
	// 启动时把 xpilot-select.log 末尾的反序列化进内存历史，让界面与文件保持一致。
	// （进程退出时内存会丢，盘里继续,这是显式的"界面不丢历史"能力。）
	s.loadSelectHistory()
	return s
}

// ---------- 内嵌 xray 实例管理 ----------

// startMain 在主端口（10808）上以给定节点启动主实例，已有实例先关闭。
func (s *Service) startMain(link string) error {
	s.stopMain()
	if PortListening(FixedMainPort) {
		// 端口仍被占用：外部残留进程（如旧版本 xray.exe），清掉
		killPortListeners(FixedMainPort)
		waitPortFree(FixedMainPort, 2*time.Second)
	}
	cfgJSON, err := buildXrayConfig(link, FixedMainPort, true, filepath.Join(s.LogDir, "xpilot-access.log"), filepath.Join(s.LogDir, "xpilot-error.log"))
	if err != nil {
		return err
	}
	inst, err := StartXrayInstance(cfgJSON)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.mainInst = inst
	s.mu.Unlock()
	return nil
}

// stopMain 关闭主实例（释放监听端口与全部连接）。
func (s *Service) stopMain() {
	s.mu.Lock()
	inst := s.mainInst
	s.mainInst = nil
	s.mu.Unlock()
	if inst != nil {
		inst.Close()
	}
}

func (s *Service) mainAlive() bool {
	return PortListening(FixedMainPort)
}

// StartMainFromConfig 用 config/config.json 里已保存的配置启动主实例
// （开机自愈：正在用的节点参数原样恢复，不重新生成配置）。
func (s *Service) StartMainFromConfig() error {
	if PortListening(FixedMainPort) {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(s.cfgDir, "config.json"))
	if err != nil {
		return fmt.Errorf("没有现成配置")
	}
	// 强制 access / error 双 "none"：日志由 xpilot 自己的 handler 写入
	// （见 xraylog.go）。旧版 config.json 里可能存着文件路径，不覆盖的话
	// 会变成 xray 与 xpilot 两套写入打架。
	var cfgMap map[string]interface{}
	if json.Unmarshal(data, &cfgMap) == nil {
		logCfg, _ := cfgMap["log"].(map[string]interface{})
		if logCfg == nil {
			logCfg = map[string]interface{}{}
			cfgMap["log"] = logCfg
		}
		logCfg["access"] = "none"
		logCfg["error"] = "none"
		data, _ = json.Marshal(cfgMap)
	}
	inst, err := StartXrayInstance(data)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.mainInst = inst
	s.mu.Unlock()
	return nil
}

// EnsurePAC 启动自检：AutoDetect 保持关闭；把 PAC 指向主端口并刷新随机串
// （被其他软件删掉时恢复，手改过的 PAC 规则也随之生效）。只在启动时做一次。
func (s *Service) EnsurePAC() {
	hideWin(exec.Command("reg", "add", RegProxyKey, "/v", "AutoDetect", "/t", "REG_DWORD", "/d", "0", "/f")).Run()
	s.pacURL = setRegistryPAC(DefaultPacMain)
}

// ---------- PAC 文件 ----------

// ---------- PAC 文件与注册表 ----------

// pacHandler 返回 html/ 目录下的 PAC 规则文件（用户自己的文件，可手动编辑，
// 改完点网页上的"刷新 PAC"或等下次运行自动刷新随机串生效）。
func (s *Service) pacHandler(file string) http.HandlerFunc {
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
func (s *Service) openRegedit() error {
	const appletsKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Applets\Regedit`
	const lastKey = `计算机\HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	hideWin(exec.Command("reg", "add", appletsKey, "/v", "LastKey", "/t", "REG_SZ", "/d", lastKey, "/f")).Run()
	return exec.Command("regedit").Start()
}

func (s *Service) handleOpenRegedit(w http.ResponseWriter, r *http.Request) {
	if err := s.openRegedit(); err != nil {
		writeJSON(w, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// ---------- HTTP ----------

func (s *Service) Listen() {
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
	mux.HandleFunc("/api/select-with-check", s.handleSelectWithCheck)
	mux.HandleFunc("/api/default-node", s.handleDefaultNode)
	mux.HandleFunc("/api/pac-refresh", s.handlePacRefresh)
	mux.HandleFunc("/api/open-regedit", s.handleOpenRegedit)
	mux.HandleFunc("/api/access-log-stream", s.handleAccessLogStream) // SSE：实时推送 access 日志
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

	addr := fmt.Sprintf("127.0.0.1:%d", DefaultWebPort)
	fmt.Printf("Web 控制台: http://127.0.0.1:%d/ ｜ PAC: %s\n", DefaultWebPort, DefaultPacMain)
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
func (s *Service) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.Lock()
		cfg, last, running, stage, pacURL := s.cfg, s.lastRun, s.running, s.stage, s.pacURL
		// 深拷贝一份：锁外 json.Encode 会遍历这个切片，
		// 若与并发 appendHistory 撞上，可能读到正在被改写的底层数组。
		hist := make([]runInfo, len(s.history))
		copy(hist, s.history)
		s.mu.Unlock()
		nextRun := "即将运行"
		if !last.Time.IsZero() {
			nextRun = last.Time.Add(time.Duration(cfg.SelectIntervalMin) * time.Minute).Format("2006-01-02 15:04:05")
		}
		writeJSON(w, map[string]interface{}{
			"cfg":          cfg,
			"defaults":     defaultConfig(),
			"running":      running,
			"stage":        stage,
			"lastRun":      last,
			"history":      hist,
			"historyLimit": HistoryLimit,
			"current":      currentMetrics(hist, last),
			"pacURL":       pacURL,
			"nextRun":      nextRun,
			"currentAddr":  currentNodeAddr(s.cfgDir),
		})
	case http.MethodPost:
		s.mu.Lock()
		cfg := s.cfg
		s.mu.Unlock()
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			writeJSON(w, map[string]interface{}{"ok": false, "error": "参数格式错误: " + err.Error()})
			return
		}
		// 第二道校验（兜底）：前端已有范围校验，这里防的是直接手改 xpilot.json。
		// Workers=0 会让 testAndFilter 的 make(chan struct{}, 0) 永久阻塞（选点死锁），
		// 负数则直接 panic；超时类参数为 0 会让所有节点瞬间失败。
		if bad := validateConfig(cfg); bad != "" {
			writeJSON(w, map[string]interface{}{"ok": false, "error": bad})
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
func (s *Service) handleFileView(w http.ResponseWriter, r *http.Request) {
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
func (s *Service) pushEvent(typ, text string) {
	data, _ := json.Marshal(map[string]string{"type": typ, "text": text})
	s.hub.publish(string(data))
}

// handleEvents SSE 长连接：建立后先推一次当前阶段（接上正在进行中的选点），
// 之后每有阶段变化/完成事件即实时下发，15 秒一次心跳防连接超时。
func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
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
func (s *Service) startSelection() {
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

// startProbeAndMaybeSelect 防重入地发起"体检+（可能）选点"。
func (s *Service) startProbeAndMaybeSelect() {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.stage = "体检当前节点"
	s.mu.Unlock()
	go s.runProbeAndMaybeSelect()
}

func (s *Service) handleRun(w http.ResponseWriter, r *http.Request) {
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

// handleSelectWithCheck 处理"带 check 的选点"按钮：先体检当前节点,按三级判断决定
// 是跳过(写体检通过记录)还是启动完整选点(体检记录+选点记录各一条)。
func (s *Service) handleSelectWithCheck(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running {
		writeJSON(w, map[string]interface{}{"ok": false, "error": "选点正在进行中"})
		return
	}
	s.startProbeAndMaybeSelect()
	writeJSON(w, map[string]interface{}{"ok": true})
}

func (s *Service) handleDefaultNode(w http.ResponseWriter, r *http.Request) {
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
	// 同步置位 running 再起 goroutine。
	// 若放进 goroutine 内部，HTTP 响应会先于置位发出，前端拉快照仍看到「空闲」→
	// 按钮不置灰 → 用户以为没点上而连点 → 两次请求都通过上面的 running 检查并发触发。
	s.mu.Lock()
	s.running = true
	s.stage = "应用默认节点中"
	s.mu.Unlock()
	s.pushEvent("stage", "应用默认节点中")

	go func() {
		start := time.Now()
		w := nodeResult{link: cfg.DefaultNodeLink, name: "my_xray"}
		if h, p, ok := extractAddrPort(cfg.DefaultNodeLink); ok {
			w.addr = net.JoinHostPort(h, p)
		}
		s.applyWinner(w, currentNodeAddr(s.cfgDir))
		// 默认节点也跑一次完整测速,让历史明细格式跟选点统一。
		// 复用 testSingleNode —— 跟选点其他节点走完全相同的代码路径
		// (独立 xray 实例 + 协议验证 + 延迟 + 下载),不另起一套。
		testRes, testErr := testSingleNode(cfg.DefaultNodeLink, xrayTestOpts{
			latencyURL:   cfg.LatencyURL,
			serverURL:    cfg.ServerURL,
			maxLatency:   time.Duration(cfg.MaxLatencyMS) * time.Millisecond,
			dlTimeout:    time.Duration(cfg.DownloadTimeoutS) * time.Second,
			downloadSize: cfg.DownloadSizeMB * 1024 * 1024,
		})
		if testErr != nil {
			// 测试失败但主实例已落地,不算"应用失败"——记录测速失败原因到历史,
			// 让用户能看到"主实例 OK 但延迟/速度测不出来"的状态。
			testRes.errText = testErr.Error()
		}
		// 与普通选点用同一 fmt.Sprintf 模板生成 Winner,前端渲染格式 100% 一致。
		// 模板见 runSelection 末段 return 处。
		// 防御 +Inf(测试失败时 latencyMs=+Inf 会渲染成 "+Infms"):替成 "—"。
		lat := fmt.Sprintf("%.0f", testRes.latencyMs)
		if math.IsInf(testRes.latencyMs, 1) {
			lat = "—"
		}
		spd := testRes.speedText
		if spd == "" {
			spd = "—"
		}
		s.mu.Lock()
		s.running = false
		s.stage = ""
		info := runInfo{
			Time:      start,
			OK:        true,
			Winner:    fmt.Sprintf("默认节点 %s（%s，%sms，%s）", testRes.name, testRes.addr, lat, spd),
			DurationS: time.Since(start).Seconds(),
		}
		info.Metrics = metricsOf(testRes)
		s.lastRun = info
		s.appendHistory(info)
		s.mu.Unlock()
		s.pushEvent("done", "已应用默认节点 my_xray")
	}()
	writeJSON(w, map[string]interface{}{"ok": true})
}

// handlePacRefresh 重写注册表 PAC（带新随机串），让手动修改过的 PAC 规则立即生效。
func (s *Service) handlePacRefresh(w http.ResponseWriter, r *http.Request) {
	s.pacURL = setRegistryPAC(DefaultPacMain)
	writeJSON(w, map[string]interface{}{"ok": true})
}

// ---------- 访问日志 SSE ----------

// handleAccessLogStream 是访问日志的 SSE 长连接：
// 1) 连上后立即把当前末尾 logMaxLines 条历史一次性补发给前端（event: history），
//    接上的那一刻表格不空白；
// 2) 之后切到 live：订阅 accessLogHub,每条新日志以 event: row 推送；
// 3) 15s 一次心跳防连接超时。
//
// 与原 GET /api/access-log 的区别：不再轮询、无空跑；日志写入的瞬间（毫秒级）
// 就推到浏览器。已删除原端点 —— 历史 dump 与 live 都走这条 SSE。
func (s *Service) handleAccessLogStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 关掉 nginx 之类中间层的缓冲（虽然本服务是直连 127.0.0.1，但加上无害）

	// 1) 先把当前已有的末尾 N 条推过去。
	//    dumpRecentAccessRows 内部就处理了「文件不存在 → 空」、「超过上限 → 截尾」、「新纪录在前」。
	histRows := dumpRecentAccessRows(filepath.Join(s.LogDir, "xpilot-access.log"), logMaxLines)
	histJSON, _ := json.Marshal(histRows)
	fmt.Fprintf(w, "event: history\ndata: %s\n\n", histJSON)
	fl.Flush()

	// 2) 订阅 live。每条新日志 publish 进来后,这里以 row event 下发。
	ch := globalAccessHub.add()
	defer globalAccessHub.remove(ch)

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case row := <-ch:
			rowJSON, _ := json.Marshal(row)
			fmt.Fprintf(w, "event: row\ndata: %s\n\n", rowJSON)
			fl.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// ---------- 选点流水线与调度 ----------

// runSelection 执行一次完整选点并记录结果（通常由 startSelection 发起）。
func (s *Service) runSelection() {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()

	start := time.Now()
	winner, m, err := s.selectBest(cfg)
	dur := time.Since(start)

	if err != nil {
		// 选点失败兜底：主实例活着就保持现状，死了用默认节点拉起
		fmt.Printf("选点失败: %v\n", err)
		s.fallbackDefaultNode()
	}

	info := runInfo{Time: start, DurationS: dur.Seconds(), Winner: winner, Metrics: m}
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
	s.appendHistory(info)
	s.mu.Unlock()
	// 落盘放在解锁之后：磁盘 IO 不阻塞服务
	s.appendSelectLog(info)
	if err != nil {
		s.pushEvent("done", "选点失败："+err.Error())
	} else {
		s.pushEvent("done", "选点完成："+winner)
	}
}

// appendHistory 追加一条选点记录，超出 HistoryLimit 后丢弃最旧的。
//
// 调用方必须已持有 s.mu。这里用 copy + 重切片而不是 append 重建：
// 容量保持不变，稳态下不再分配新底层数组。
func (s *Service) appendHistory(info runInfo) {
	s.history = append(s.history, info)
	if n := len(s.history) - HistoryLimit; n > 0 {
		copy(s.history, s.history[n:])
		s.history = s.history[:HistoryLimit]
	}
}

// appendSelectLog 把一条选点记录以单行 JSON 追加到 logs/xpilot-select.log。
//
// 与内存里的 history 是两回事：history 是「给界面看的窗口」（有 HistoryLimit 上限，
// 启动时从本文件加载 —— 见 loadSelectHistory），本文件是完整历史，只追加、
// 不轮转、不裁剪。
//
// ★ 必须在释放 s.mu 之后调用 —— 磁盘 IO 不该阻塞整个服务。
// 写失败只记 stderr，不影响选点流程本身。
func (s *Service) appendSelectLog(info runInfo) {
	b, err := json.Marshal(info)
	if err != nil {
		fmt.Printf("选点日志序列化失败: %v\n", err)
		return
	}
	path := filepath.Join(s.LogDir, "xpilot-select.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Printf("选点日志打开失败: %v\n", err)
		return
	}
	defer f.Close()
	// 每条一行；Write 失败也无需中断流程
	if _, err := f.Write(append(b, '\n')); err != nil {
		fmt.Printf("选点日志写入失败: %v\n", err)
	}
}

// loadSelectHistory 启动时把 logs/xpilot-select.log 末尾若干行反序列化进 s.history,
// 让界面与文件保持一致（界面最多展示 HistoryLimit 条,旧条目自然被截断）。
//
// 坏行（JSON 解析失败、写文件中途崩溃留下的半截行）一律跳过、记 stderr,
// 不阻塞启动 —— 启动期遇到一条半截 JSON 整个 xpilot 起不来,代价太高。
//
// 性能:文件可能很大,只关心尾部。粗略策略是取末尾 maxLinesToLoad 行,
// 反序列化后截到 HistoryLimit。maxLinesToLoad 给一个比 HistoryLimit 大几倍的
// 余量,避免坏行集中在末尾时丢太多好行。
func (s *Service) loadSelectHistory() {
	path := filepath.Join(s.LogDir, "xpilot-select.log")
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "读取选点历史失败: %v\n", err)
		}
		return
	}
	text := strings.TrimRight(string(data), "\r\n")
	if text == "" {
		return
	}
	lines := strings.Split(text, "\n")
	const maxLinesToLoad = 4 * HistoryLimit
	if len(lines) > maxLinesToLoad {
		lines = lines[len(lines)-maxLinesToLoad:]
	}
	hist := make([]runInfo, 0, len(lines))
	for _, ln := range lines {
		var info runInfo
		if err := json.Unmarshal([]byte(ln), &info); err != nil {
			fmt.Fprintf(os.Stderr, "选点历史坏行已跳过: %v (%q)\n", err, ln)
			continue
		}
		hist = append(hist, info)
	}
	// 复用 appendHistory 的截断语义:超过 HistoryLimit 就丢最旧的。
	if n := len(hist) - HistoryLimit; n > 0 {
		copy(hist, hist[n:])
		hist = hist[:HistoryLimit]
	}
	s.history = hist
}

// currentMetrics 取「当前上线节点」的指标，供卡片展示。
//
// 优先从历史末尾往前找第一条带 metrics 的 —— 历史里的指标是选点当时实测的，
// 最贴近「这个节点上线时的表现」。历史为空（刚重启）才退回 lastRun。
func currentMetrics(hist []runInfo, last runInfo) *metrics {
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Metrics != nil {
			return hist[i].Metrics
		}
	}
	return last.Metrics
}

// setStage 更新选点阶段（供控制台显示），并实时推送给所有已连接的页面。
func (s *Service) setStage(t string) {
	s.mu.Lock()
	s.stage = t
	s.mu.Unlock()
	s.pushEvent("stage", t)
}

// selectBest 完整流水线：订阅 → 过滤去重 → TCP → xray 实测 → 热替换落地 → 报告。
func (s *Service) selectBest(cfg appConfig) (winner string, m *metrics, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	if len(cfg.SubscribeURLs) == 0 {
		return "", nil, fmt.Errorf("未配置订阅地址")
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
			RunReport.addSub(u, 0, err.Error())
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
		RunReport.addSub(u, len(links), "")
		totalRaw += len(links)
		allLinks = append(allLinks, links...)
	}
	if totalRaw == 0 {
		return "", nil, fmt.Errorf("没有获取到任何节点（%d 个订阅失败）", failed)
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
	RunReport.setFetch(protoName(cfg.Proto), totalRaw, len(deduped), unparsed, failed)

	tested := deduped
	if cfg.TCPTest {
		s.setStage("TCP 连通测试中")
		fmt.Printf("开始 TCP 连通测试（超时 1s，并发 %d）...\n", cfg.Workers)
		var tcpStats []tcpStat
		tested, tcpStats = testAndFilter(deduped, time.Second, cfg.Workers)
		RunReport.setNodes(deduped, tcpStats)
		if len(tested) == 0 {
			return "", nil, fmt.Errorf("所有节点均无法连通")
		}
	} else {
		RunReport.setNodes(deduped, nil)
	}

	listPath := filepath.Join(s.LogDir, "node.list.txt")
	if err := os.WriteFile(listPath, []byte(strings.Join(tested, "\n")+"\n"), 0o644); err != nil {
		return "", nil, err
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
		return "", nil, err
	}
	RunReport.setXray(rows)
	w, ok := saveBestAndPick(rows, s.LogDir, curAddr)
	if !ok {
		return "", nil, fmt.Errorf("没有任何节点通过 xray 实测")
	}
	RunReport.setWinner(w.link)
	RunReport.writeHTML(s.htmlDir)
	s.applyWinner(w, curAddr)
	return fmt.Sprintf("%s（%s，%.0fms，%s）", w.name, w.addr, w.latencyMs, w.speedText), metricsOf(w), nil
}

// metricsOf 把选中的节点结果抽成结构化指标。
//
// latencyMs 为 +Inf（验证失败）时不算有效延迟，返回 0 —— 前端据此显示「—」，
// 而不是显示出 `+Inf` 这种没法看的东西。packetLoss 验证失败时为 1（100%），
// 前端按"丢包率：1"展示（与延迟为 0 → "—" 不同：丢包率即使 100% 仍是合法数）。
func metricsOf(w nodeResult) *metrics {
	ms := int64(0)
	if !math.IsInf(w.latencyMs, 0) {
		ms = int64(w.latencyMs)
	}
	return &metrics{
		Name:       w.name,
		Addr:       w.addr,
		LatencyMs:  ms,
		PacketLoss: w.packetLoss,
		SpeedBps:   w.speedBps,
		SpeedText:  w.speedText,
	}
}

// Loops 常驻循环：健康检查（主实例掉线就用现有配置原地拉起）+ 到期触发体检或选点。
// 计划任务只需负责保证本进程开机自启，其余不依赖它。
//
// 调度策略：每 SelectIntervalMin 分钟触发 runProbeAndMaybeSelect，它内部按三级
// 决定是跳过本轮（写一条体检通过记录）还是走完整选点（写体检未达标记录后再写选点记录）。
// 也就是说"自动选点间隔"现在指的就是体检+（必要时）选点的总调度间隔——到时间不一定选点。
func (s *Service) Loops() {
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
			if err := s.StartMainFromConfig(); err == nil {
				fmt.Println("看门狗：主实例掉线，已用现有配置重新拉起")
			}
		}
		// 到期触发体检/选点（lastRun 为零值时视为从未运行，启动后立即执行一轮）
		s.mu.Lock()
		due := time.Since(s.lastRun.Time) >= time.Duration(cfg.SelectIntervalMin)*time.Minute
		s.mu.Unlock()
		if due {
			s.runProbeAndMaybeSelect()
		}
	}
}

// applyWinner 把最优节点落地：主实例运行中 → 热替换其 proxy 出站（监听端口与
// 实例保持不动，旧出站延迟关闭让在途连接排空，浏览器零感知）；主实例未运行 →
// 直接以该节点启动主实例。落地后回写 config.json 供开机自愈与粘性判断使用。
func (s *Service) applyWinner(w nodeResult, curAddr string) {
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

	if err := verifyOutbound(FixedMainPort); err != nil {
		fmt.Printf("警告: 出网检查失败: %v\n", err)
	}
	// 回写当前节点的启动配置（config.json 的 outbounds 已指向新节点）
	if cfgJSON, err := buildXrayConfig(w.link, FixedMainPort, true, filepath.Join(s.LogDir, "xpilot-access.log"), filepath.Join(s.LogDir, "xpilot-error.log")); err == nil {
		os.WriteFile(filepath.Join(s.cfgDir, "config.json"), cfgJSON, 0o644)
	}
}

// swapMainOutbound 热替换主实例的 proxy 出站（委托给 xraycore.go 的接入层）。
func (s *Service) swapMainOutbound(link string) error {
	if s.mainInst == nil {
		return fmt.Errorf("主实例未运行")
	}
	cfgJSON, err := buildXrayConfig(link, FixedMainPort, true, filepath.Join(s.LogDir, "xpilot-access.log"), filepath.Join(s.LogDir, "xpilot-error.log"))
	if err != nil {
		return err
	}
	return s.mainInst.swapOutbound(cfgJSON)
}

// fallbackDefaultNode 选点失败时的兜底：主实例还在跑就保持现状（它此前验证过），
// 否则用内置默认节点拉起。
func (s *Service) fallbackDefaultNode() {
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

// ---------- 体检（选点前的快速健康检查）----------

// currentNodeLink 从 config.json 反解出当前主实例正在用的节点链接（用于体检时填 metrics.name）。
// 找不到（主实例没起来 / 配置文件异常）返回 ""。
// 当前实现不依赖 name:probeCurrentNode 返回的 metrics 已经带 addr,前端可照常展示。
// 函数保留以便后续反解 xray 配置 JSON 拿原始分享链接用。
func (s *Service) currentNodeLink() string {
	_, err := os.ReadFile(filepath.Join(s.cfgDir, "config.json"))
	if err != nil {
		return ""
	}
	return "" // 占位:见上方注释,目前 metrics 由 probeCurrentNode 提供
}

// probeAndDecide 体检当前节点并做三级判断:
//
//	快检通过(延迟<ProbeLatencyFastMs 且 丢包<ProbeLossFast) → 跳过,Decision="fast",不测速
//	全检通过(延迟<ProbeLatencyFullMs 且 丢包≤ProbeLossFull 且 速度≥ProbeMinSpeedBps) → 跳过,Decision="full"
//	体检失败(xray 不通等) → 触发完整选点,Decision="fail"
//	体检未达标 → 触发完整选点,Decision="trigger"
//
// 关键路径优化:fast 路径只跑延迟+丢包（约 3-6 秒）,full 路径再补 5 MB 测速（再 1-3 秒）。
// 90% 健康情况下 fast 通过,根本不走 5 MB 下载——白白浪费 1-3 秒并污染历史里的速度数据。
//
// 返回 runInfo：失败/触发的也返回 info（写一条历史记录）,决策结果通过 info.Decision 表达。
func (s *Service) probeAndDecide(cfg appConfig) runInfo {
	start := time.Now()
	s.setStage("体检当前节点（延迟+丢包）")

	opt := xrayTestOpts{
		serverURL:  cfg.ServerURL,
		latencyURL: cfg.LatencyURL,
		maxLatency: time.Duration(cfg.MaxLatencyMS) * time.Millisecond,
		dlTimeout:  time.Duration(cfg.DownloadTimeoutS) * time.Second,
	}

	// 第一步永远走 fast（延迟+丢包）
	r, probeErr := probeCurrentNodeFast(FixedMainPort, opt)

	info := runInfo{
		Time:      start,
		DurationS: 0, // 在最后填充
		Winner:    fmt.Sprintf("体检 %s（%.0fms, 丢包 %d%%）",
			currentNodeAddr(s.cfgDir), r.latencyMs, int(r.packetLoss*100)),
		Metrics: metricsOf(r),
	}

	if probeErr != nil {
		info.Decision = "fail"
		info.Error = probeErr.Error()
		info.DurationS = time.Since(start).Seconds()
		fmt.Printf("体检失败,触发完整选点: %v\n", probeErr)
		return info
	}

	// 第一级（快检）：延迟+丢包都极佳 → 直接跳过,不测速
	if r.latencyMs < float64(cfg.ProbeLatencyFastMs) && r.packetLoss < cfg.ProbeLossFast {
		info.Decision = "fast"
		info.OK = true
		info.DurationS = time.Since(start).Seconds()
		fmt.Printf("快检通过: 延迟 %.0fms < %dms, 丢包 %d%% < %.1f%% → 跳过本轮选点（不测速）\n",
			r.latencyMs, cfg.ProbeLatencyFastMs, int(r.packetLoss*100), cfg.ProbeLossFast*100)
		return info
	}

	// 第二级（全检）：延迟+丢包尚可,需要测速 —— 这里才补 5 MB 下载
	if r.latencyMs < float64(cfg.ProbeLatencyFullMs) && r.packetLoss <= cfg.ProbeLossFull {
		s.setStage("体检当前节点（延迟+丢包+测速）")
		sp, dlErr := probeDownload(FixedMainPort, opt.serverURL, 5*1024*1024, opt.dlTimeout)
		if dlErr == nil {
			r.speedBps = sp
			r.speedText = fmtSpeed(sp)
			info.Metrics = metricsOf(r) // 把速度补进 metrics
			if sp >= cfg.ProbeMinSpeedBps {
				info.Decision = "full"
				info.OK = true
				info.DurationS = time.Since(start).Seconds()
				fmt.Printf("全检通过: 延迟 %.0fms < %dms, 丢包 %d%% ≤ %.1f%%, 速度 %s ≥ 阈值 → 跳过本轮选点\n",
					r.latencyMs, cfg.ProbeLatencyFullMs, int(r.packetLoss*100), cfg.ProbeLossFull*100,
					fmtSpeed(sp))
				return info
			}
			// 速度不达标：触发选点
			fmt.Printf("全检未过: 速度 %s < 阈值 %s → 触发完整选点\n",
				fmtSpeed(sp), fmtSpeed(cfg.ProbeMinSpeedBps))
			info.Decision = "trigger"
			info.DurationS = time.Since(start).Seconds()
			return info
		}
		// 测速本身失败（网络抽风）—— 也走触发,避免误判
		fmt.Printf("全检测速失败: %v → 触发完整选点\n", dlErr)
		info.Decision = "trigger"
		info.DurationS = time.Since(start).Seconds()
		return info
	}

	// 任一不达标：触发选点
	fmt.Printf("体检未达标: 延迟 %.0fms / 丢包 %d%% → 触发完整选点\n",
		r.latencyMs, int(r.packetLoss*100))
	info.Decision = "trigger"
	info.DurationS = time.Since(start).Seconds()
	return info
}

// runProbeAndMaybeSelect 执行体检并按决策走后续流程（写入历史 + 必要时启动完整选点）。
// 用于自动调度（Loops）和手动触发（带 check 的选点）。
func (s *Service) runProbeAndMaybeSelect() {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()

	info := s.probeAndDecide(cfg)

	// 先写体检记录到历史——决策细节不论,这次"动作"必须留痕
	s.mu.Lock()
	s.running = false
	s.stage = ""
	s.lastRun = info
	s.appendHistory(info)
	s.mu.Unlock()
	s.appendSelectLog(info)

	switch info.Decision {
	case "fast", "full":
		// 跳过选点：推 done 事件,前端会看到「体检通过」
		label := "快检"
		if info.Decision == "full" {
			label = "全检"
		}
		s.pushEvent("done", label+"通过，跳过本轮选点")
	case "fail", "trigger":
		// 触发完整选点
		s.startSelection()
	}
}
