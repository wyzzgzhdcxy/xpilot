package app

// PWA 相关端点的集成测试。
//
// 起一个 httptest 服务跑真实的路由表，验证三件事：
//  1. manifest / sw.js / 图标的响应码与 Content-Type 正确（错了浏览器就不认这个 PWA）
//  2. manifest 里引用的每个图标都真的能取到（清单与实际文件对不上是最常见的坑）
//  3. 图标白名单拦得住非图标文件（/icons/ 不能变成第二个 html 目录入口）

import (
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pwaFiles 是 PWA 运行必需的文件（缺一个浏览器就会判定"不可安装"）。
var pwaFiles = []string{
	"console.html",
	"manifest.webmanifest",
	"sw.js",
	"icon-192.png",
	"icon-512.png",
	"icon-maskable-512.png",
	"apple-touch-icon.png",
	"favicon-32.png",
}

// newPWAServer 搭一个跑着真实路由表的测试服务。
// 用临时目录当数据目录，把项目 html/ 下的 PWA 文件复制进去，
// 这样测的是"释放到数据目录后能否被正常访问"这条真实链路。
func newPWAServer(t *testing.T) *httptest.Server {
	t.Helper()
	base := t.TempDir()
	htmlDir := filepath.Join(base, "html")
	if err := os.MkdirAll(htmlDir, 0o755); err != nil {
		t.Fatalf("创建临时 html 目录失败: %v", err)
	}
	for _, name := range pwaFiles {
		src := filepath.Join("..", "html", name)
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("读取项目文件 %s 失败: %v", src, err)
		}
		if err := os.WriteFile(filepath.Join(htmlDir, name), data, 0o644); err != nil {
			t.Fatalf("写入临时文件 %s 失败: %v", name, err)
		}
	}
	s := &Service{htmlDir: htmlDir}
	srv := httptest.NewServer(loopbackOnly(s.newMux()))
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s 失败: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取 %s 响应体失败: %v", path, err)
	}
	return resp, body
}

func TestPWAManifest(t *testing.T) {
	srv := newPWAServer(t)
	resp, body := get(t, srv, "/manifest.webmanifest")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest 状态码 = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/manifest+json") {
		t.Errorf("manifest Content-Type = %q，期望包含 application/manifest+json", ct)
	}

	var m struct {
		Name      string `json:"name"`
		ShortName string `json:"short_name"`
		StartURL  string `json:"start_url"`
		Scope     string `json:"scope"`
		Display   string `json:"display"`
		Theme     string `json:"theme_color"`
		Icons     []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("manifest 不是合法 JSON: %v", err)
	}
	if m.Name == "" || m.ShortName == "" {
		t.Errorf("manifest 缺少 name/short_name: %+v", m)
	}
	// start_url 与 scope 必须是根路径：控制台的页面在 '/',
	// 作用域收窄的话 PWA 启动后拿不到 /api/* 的授权范围。
	if m.StartURL != "/" {
		t.Errorf("manifest start_url = %q，期望 \"/\"", m.StartURL)
	}
	if m.Scope != "/" {
		t.Errorf("manifest scope = %q，期望 \"/\"", m.Scope)
	}
	if m.Display != "standalone" {
		t.Errorf("manifest display = %q，期望 standalone", m.Display)
	}
	if m.Theme == "" {
		t.Error("manifest 缺少 theme_color")
	}

	// 至少要有 192 与 512 两种尺寸，Chrome 的可安装性检查要求 192+512 齐备。
	sizes := map[string]bool{}
	for _, ic := range m.Icons {
		sizes[ic.Sizes] = true
	}
	if !sizes["192x192"] {
		t.Error("manifest 缺少 192x192 图标")
	}
	if !sizes["512x512"] {
		t.Error("manifest 缺少 512x512 图标")
	}
}

func TestPWAManifestIconsAreReachable(t *testing.T) {
	srv := newPWAServer(t)
	_, body := get(t, srv, "/manifest.webmanifest")

	var m struct {
		Icons []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("manifest 不是合法 JSON: %v", err)
	}
	if len(m.Icons) == 0 {
		t.Fatal("manifest 没有声明任何图标")
	}
	// 清单与磁盘对不上是最常见的坑：浏览器会去取，取不到就判定不可安装。
	for _, ic := range m.Icons {
		resp, _ := get(t, srv, ic.Src)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("manifest 引用的图标 %s 状态码 = %d，期望 200", ic.Src, resp.StatusCode)
		}
	}
}

func TestPWAServiceWorker(t *testing.T) {
	srv := newPWAServer(t)
	resp, body := get(t, srv, "/sw.js")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sw.js 状态码 = %d，期望 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("sw.js Content-Type = %q，期望包含 javascript", ct)
	}
	// SW 脚本被缓存住 = 用户永远拿不到新版本，PWA 的更新链就断了。
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("sw.js Cache-Control = %q，期望包含 no-cache", cc)
	}
	if allowed := resp.Header.Get("Service-Worker-Allowed"); allowed != "/" {
		t.Errorf("sw.js Service-Worker-Allowed = %q，期望 \"/\"", allowed)
	}

	js := string(body)
	for _, want := range []string{"install", "activate", "fetch", "skipWaiting"} {
		if !strings.Contains(js, want) {
			t.Errorf("sw.js 缺少 %q 相关逻辑", want)
		}
	}
	// API 必须完全绕开缓存：/api/events 是 SSE 长连接，
	// 一旦被缓存或被 respondWith 接管，实时推送会直接挂死。
	if !strings.Contains(js, "startsWith('/api/')") {
		t.Error("sw.js 没有显式绕开 /api/，SSE 可能被缓存接管")
	}
}

func TestPWAIcons(t *testing.T) {
	srv := newPWAServer(t)
	cases := []struct {
		path string
		size int
	}{
		{"/icons/icon-192.png", 192},
		{"/icons/icon-512.png", 512},
		{"/icons/icon-maskable-512.png", 512},
		{"/icons/apple-touch-icon.png", 180},
		{"/icons/favicon-32.png", 32},
	}
	for _, c := range cases {
		resp, body := get(t, srv, c.path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s 状态码 = %d，期望 200", c.path, resp.StatusCode)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
			t.Errorf("%s Content-Type = %q，期望 image/png", c.path, ct)
		}
		img, err := png.Decode(strings.NewReader(string(body)))
		if err != nil {
			t.Errorf("%s 不是合法 PNG: %v", c.path, err)
			continue
		}
		b := img.Bounds()
		if b.Dx() != c.size || b.Dy() != c.size {
			t.Errorf("%s 尺寸 = %dx%d，期望 %dx%d", c.path, b.Dx(), b.Dy(), c.size, c.size)
		}
	}
}

func TestPWAIconWhitelist(t *testing.T) {
	srv := newPWAServer(t)
	// 非图标文件不能从 /icons/ 拿到，否则这个路径等于把整个 html 目录再开一个入口。
	for _, p := range []string{"/icons/geoip.dat", "/icons/pac", "/icons/console.html", "/icons/nope.png"} {
		resp, _ := get(t, srv, p)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s 状态码 = %d，期望 404", p, resp.StatusCode)
		}
	}
}

func TestPWAConsoleReferences(t *testing.T) {
	srv := newPWAServer(t)
	resp, body := get(t, srv, "/")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("控制台状态码 = %d，期望 200", resp.StatusCode)
	}
	html := string(body)
	for _, want := range []string{
		`href="/manifest.webmanifest"`,
		`register('/sw.js'`,
		`name="theme-color"`,
		`name="apple-mobile-web-app-capable"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("控制台页面缺少 %s", want)
		}
	}
}

// TestPWAReleaseEmbedded 验证 PWA 资源会被释放到用户数据目录。
//
// PWA 资源属于"程序文件"（不是用户文件），所以每次启动都要用 exe 内嵌版本覆盖 ——
// 否则升级 xpilot 后浏览器拿到的还是旧 SW，版本更新链就断了。
func TestPWAReleaseEmbedded(t *testing.T) {
	base := t.TempDir()
	ReleaseEmbedded(base, os.DirFS(".."))

	for _, name := range []string{
		"manifest.webmanifest", "sw.js", "icon-192.png", "icon-512.png",
		"icon-maskable-512.png", "apple-touch-icon.png", "favicon-32.png",
	} {
		if _, err := os.Stat(filepath.Join(base, "html", name)); err != nil {
			t.Errorf("PWA 资源 %s 未被释放: %v", name, err)
		}
	}

	// 程序文件必须每次覆盖：先写脏内容再释放，应当被内嵌版本盖回去。
	dirty := filepath.Join(base, "html", "sw.js")
	if err := os.WriteFile(dirty, []byte("DIRTY"), 0o644); err != nil {
		t.Fatal(err)
	}
	ReleaseEmbedded(base, os.DirFS(".."))
	got, _ := os.ReadFile(dirty)
	if string(got) == "DIRTY" {
		t.Error("sw.js 是程序文件，释放时应被内嵌版本覆盖")
	}
}
