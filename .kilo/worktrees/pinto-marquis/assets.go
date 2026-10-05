package main

// 内嵌资源与用户数据目录：html/ 下的全部资源（控制台页面、PAC、geoip/geosite 资产）
// 编译进 exe，首次运行释放到用户数据目录的 html/ 文件夹；geo 资产也从那里加载。
// 配置、日志等运行期文件同样集中在数据目录，exe 保持单文件、可在任意位置离线运行。

import (
	"embed"
	"io"
	"os"
	"path/filepath"
)

// 随 exe 分发的资源文件（位于项目 html/ 目录，构建时必须齐全），统一释放到 html/。
// config.json / xpilot.json / xpilot-report.html 是运行期生成的状态文件，不内嵌：
// xpilot.json 缺失时由 loadConfig 用默认参数创建，config.json 由选点成功后生成。
type embeddedFile struct {
	name string // 项目 html/ 下的文件名
	sub  string // 数据目录里的释放子目录（空为根部）
}

var embeddedFiles = []embeddedFile{
	{"console.html", "html"},
	{"pac", "html"},
	{"geoip.dat", "html"},
	{"geosite.dat", "html"},
}

//go:embed html/console.html html/pac html/geoip.dat html/geosite.dat
var embeddedFS embed.FS

// defaultPacScript 构建 html/pac 缺失时释放的起始模板（分流口径与 xray 路由接近），
// 属于用户文件，只在缺失时释放，之后由用户自行维护。
const defaultPacScript = `function FindProxyForURL(url, host) {
  host = host.toLowerCase();
  if (isPlainHostName(host) ||
      shExpMatch(host, "localhost") ||
      shExpMatch(host, "127.*") ||
      shExpMatch(host, "192.168.*") ||
      shExpMatch(host, "10.*") ||
      shExpMatch(host, "*.cn")) {
    return "DIRECT";
  }
  return "PROXY 127.0.0.1:10808";
}
`

// appDataDir 返回程序的用户数据目录：Windows 上是 %AppData%\xpilot。
// exe 可以放在任何位置运行，所有运行期文件（html/、config/、logs/）都集中在这里。
func appDataDir() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "xpilot"), nil
}

// releaseEmbedded 把内嵌资源释放到用户数据目录。
// console.html 属于程序文件，每次启动都用 exe 内嵌版本覆盖，保证页面与程序版本一致；
// 其余文件（pac、geo 资产）只在缺失时写出，不覆盖用户修改。
func releaseEmbedded(baseDir string) {
	for _, ef := range embeddedFiles {
		data, err := embeddedFS.ReadFile("html/" + ef.name)
		if err != nil {
			continue // 构建时未包含该文件
		}
		os.MkdirAll(filepath.Join(baseDir, ef.sub), 0o755)
		target := filepath.Join(baseDir, ef.sub, ef.name)
		if ef.name == "console.html" {
			os.WriteFile(target, data, 0o644)
			continue
		}
		writeIfMissing(target, data)
	}
	// 构建里没带 pac 时（用户文件，可不入库），释放起始模板保证 PAC 服务可用
	writeIfMissing(filepath.Join(baseDir, "html", "pac"), []byte(defaultPacScript))
}

func writeIfMissing(path string, data []byte) {
	if _, err := os.Stat(path); err == nil {
		return
	}
	os.WriteFile(path, data, 0o644)
}

// migrateLegacyFiles 把旧版本散落在 exe 目录里的运行文件迁到用户数据目录。
// 兼容 exe 根目录、config/、html/、logs/ 四个历史位置；目标已存在时不覆盖。
// console.html 不迁移：它已内嵌进 exe，以 exe 内的版本为准，缺失时自动重新释放。
func migrateLegacyFiles(exeDir, baseDir string) {
	type group struct {
		names []string
		sub   string
	}
	plan := []group{
		{[]string{"geoip.dat", "geosite.dat"}, "html"}, // geo 资产与页面资源同在 html/
		{[]string{"xpilot.json", "config.json", "config-slave.json"}, "config"},
		{[]string{"pac"}, "html"},
		{[]string{"node.list.txt", "node.best.txt", "xpilot.log", "xray-access.log", "xray-error.log", "xray-auto.log", "xray-slave.log"}, "logs"},
	}
	var srcDirs []string
	for _, sub := range []string{"", "config", "html", "logs"} {
		srcDirs = append(srcDirs, filepath.Join(exeDir, sub))
	}
	for _, g := range plan {
		for _, name := range g.names {
			for _, srcDir := range srcDirs {
				if moveIfExist(filepath.Join(srcDir, name), filepath.Join(baseDir, g.sub, name)) {
					break // 该文件已处理（迁走或目标已有），不再尝试其他来源位置
				}
			}
		}
	}
}

// moveIfExist 把 src 移动到 dst；目标已存在则不动，src 不存在返回 false。
// 跨盘（如 exe 在 D:、数据目录在 C:）用复制+删源实现移动。
func moveIfExist(src, dst string) bool {
	if _, err := os.Stat(src); err != nil {
		return false
	}
	if _, err := os.Stat(dst); err == nil {
		return true
	}
	os.MkdirAll(filepath.Dir(dst), 0o755)
	in, err := os.Open(src)
	if err != nil {
		return true
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return true
	}
	_, err = io.Copy(out, in)
	out.Close()
	if err != nil {
		return true
	}
	in.Close()
	os.Remove(src)
	return true
}
