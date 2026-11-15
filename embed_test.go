package main

// 内嵌资源的完整性测试。
//
// 为什么需要这条测试：//go:embed 的文件清单是手写的字符串列表，
// 而 app 包的 embeddedFiles 是另一份独立的列表。两边只要有一处漏写，
// 编译照样通过（embed 只是少内嵌了一个文件），但程序启动时那个资源就**静默缺失**：
//
//   - 少了 console.html → 控制台 404
//   - 少了 manifest / sw.js / 图标 → 浏览器判定"不可安装"，而且没有任何报错
//
// 所以这里直接对着 embeddedFS 点名检查，把这类错误挡在构建期。

import (
	"testing"

	"xpilot/app"
)

// requiredEmbedded 是运行必需的内嵌资源。
// 与 app 包的 embeddedFiles 对应：程序文件（页面、PWA 资源）每次启动覆盖，
// 用户文件（pac、geo 资产）缺失时释放。
var requiredEmbedded = []string{
	"html/console.html",
	"html/pac",
	"html/geoip.dat",
	"html/geosite.dat",
	"html/manifest.webmanifest",
	"html/sw.js",
	"html/icon-192.png",
	"html/icon-512.png",
	"html/icon-maskable-512.png",
	"html/apple-touch-icon.png",
	"html/favicon-32.png",
}

func TestEmbeddedAssetsPresent(t *testing.T) {
	for _, name := range requiredEmbedded {
		f, err := embeddedFS.Open(name)
		if err != nil {
			t.Errorf("内嵌资源缺失 %s: %v（检查 main.go 的 //go:embed 清单）", name, err)
			continue
		}
		f.Close()
	}
}

// TestEmbeddedCoversReleaseList 验证 //go:embed 覆盖了 app 包要释放的全部文件。
//
// 这条比上一个更强：它直接拿 app 包的释放清单来比对，
// 以后往 embeddedFiles 里加新文件却忘了改 //go:embed，会立刻在这里报错。
func TestEmbeddedCoversReleaseList(t *testing.T) {
	for _, name := range app.EmbeddedFileNames() {
		path := "html/" + name
		if _, err := embeddedFS.Open(path); err != nil {
			t.Errorf("app 要释放 %s，但 //go:embed 里没有它: %v（检查 main.go 的 //go:embed 清单）", name, err)
		}
	}
}
