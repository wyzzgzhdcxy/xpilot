// xpilot 常驻代理管家入口。
//
// 业务实现见 app 包；本文件只负责拉起：
//   - 内嵌资源与旧文件迁移（app.ReleaseEmbedded / app.MigrateLegacyFiles）
//   - 用户数据目录、html/config/logs 目录创建
//   - 接管 stdout 到 logs/xpilot.log（windowsgui 模式无控制台）
//   - Web 控制台 + PAC + 调度循环（app.Service）
//   - 看门狗 / 体检 / 选点主入口（svc.Listen / svc.Loops / svc.EnsurePAC）
//
// 内嵌资源（//go:embed 不支持 .. 路径）必须留在 main 包：html/ 目录与本文件同级，
// 把 embed.FS 通过 app.EmbeddedAssets 暴露给 app 包使用。
package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"xpilot/app"
)

//go:embed html/console.html html/pac html/geoip.dat html/geosite.dat
var embeddedFS embed.FS

func main() {
	app.RunReport.Started = time.Now()
	exePath, err := os.Executable()
	if err != nil {
		fmt.Printf("无法获取程序路径: %v\n", err)
		os.Exit(1)
	}
	exeDir := filepath.Dir(exePath)

	baseDir, err := app.AppDataDir()
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

	if f, err := os.OpenFile(filepath.Join(baseDir, "logs", "xpilot.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		os.Stdout = f
		defer f.Close()
	}
	fmt.Printf("运行数据目录: %s\n", baseDir)

	app.MigrateLegacyFiles(exeDir, baseDir)
	app.ReleaseEmbedded(baseDir, embeddedFS)

	for _, name := range []string{"geoip.dat", "geosite.dat"} {
		if _, err := os.Stat(filepath.Join(baseDir, "html", name)); err != nil {
			fmt.Printf("警告: 用户数据目录 html 下缺少 %s，xray 分流将无法启动；请把该文件放入 %s\\html\n", name, baseDir)
		}
	}

	svc := app.NewService(baseDir)

	app.SetupXrayAssetLocation(filepath.Join(baseDir, "html"))

	if err := app.SetupXrayLogging(svc.LogDir); err != nil {
		fmt.Printf("警告: 接管 xray 日志失败，访问日志将不可用: %v\n", err)
	}

	svc.EnsurePAC()
	if !app.PortListening(app.FixedMainPort) {
		if err := svc.StartMainFromConfig(); err == nil {
			fmt.Println("已用现有配置恢复主实例")
		}
	}

	go svc.Listen()
	go svc.Loops()

	select {}
}