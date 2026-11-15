// gen_icons —— 生成 PWA 图标（PNG）。
//
// 用法：
//
//	go run ./tools/gen_icons
//
// 图形是程序化绘制的（渐变圆角方块 + 白色纸飞机），不依赖任何外部素材：
// 换机器、换 Go 版本都能复现出同样的字节，也不用往仓库里塞二进制设计稿。
//
// 输出到 html/ 目录（与 console.html 同级），由 exe 内嵌后释放到用户数据目录。
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

// ---- 几何 ----

type pt struct{ x, y float64 }

// polyInside 用奇偶规则（even-odd）判断点是否落在多边形内。
// 用它而不是"射线法 + 顶点计数"，是因为纸飞机是凹多边形，
// 奇偶规则对凹多边形的挖空处理天然正确。
func polyInside(poly []pt, x, y float64) bool {
	inside := false
	n := len(poly)
	for i, j := 0, n-1; i < n; j, i = i, i+1 {
		xi, yi := poly[i].x, poly[i].y
		xj, yj := poly[j].x, poly[j].y
		if (yi > y) != (yj > y) {
			// 边 (i,j) 与水平线 y 相交于 x
			if x < xi+(y-yi)/(yj-yi)*(xj-xi) {
				inside = !inside
			}
		}
	}
	return inside
}

// inRoundRect 判断点是否在圆角矩形内。
// 用圆角矩形的有符号距离场：中间的平台区 dx=dy=0，四个角的判定退化成圆。
func inRoundRect(x, y, size, r float64) bool {
	dx := math.Max(math.Max(r-x, x-(size-r)), 0)
	dy := math.Max(math.Max(r-y, y-(size-r)), 0)
	return dx*dx+dy*dy <= r*r
}

func lerp(a, b uint8, t float64) uint8 {
	return uint8(math.Round(float64(a) + (float64(b)-float64(a))*t))
}

// ---- 纸飞机 ----

// planeBase 是纸飞机（Material「send」箭头）在 24×24 网格上的顶点。
// 顺序：外轮廓顺时针，再折回内侧挖出尾部的缺口（奇偶规则自动处理）。
var planeBase = []pt{
	{2, 21}, {23, 12}, {2, 3}, {2, 10}, {17, 12}, {2, 14},
}

// planePoly 把 24 网格上的纸飞机换算到单位画布（0~1），按宽度占比缩放并居中。
//
// 图形在 24 网格里横跨 x:[2,23]（宽 21）、y:[3,21]（高 18），
// 中心是 (12.5, 12)；widthRatio 表示"图形占画布宽度的比例"。
func planePoly(widthRatio float64) []pt {
	s := widthRatio / 21.0
	out := make([]pt, 0, len(planeBase))
	for _, p := range planeBase {
		out = append(out, pt{
			x: 0.5 + (p.x-12.5)*s,
			y: 0.5 + (p.y-12.0)*s,
		})
	}
	return out
}

// ---- 绘制 ----

// spec 描述一个图标的外观与输出。
type spec struct {
	name   string  // 输出文件名
	size   int     // 边长（像素）
	radius float64 // 圆角半径（像素）；0 表示满幅不透明（maskable / iOS）
	plane  float64 // 纸飞机占画布宽度的比例
	opaque bool    // true 表示背景铺满（无透明圆角）
}

// draw 渲染一个图标：竖直渐变底 + 白色纸飞机，3×3 超采样做抗锯齿。
func draw(sp spec) *image.RGBA {
	const ss = 3 // 每个像素的每轴超采样数
	img := image.NewRGBA(image.Rect(0, 0, sp.size, sp.size))
	plane := planePoly(sp.plane)
	inv := 1.0 / float64(ss*ss)
	for py := 0; py < sp.size; py++ {
		for px := 0; px < sp.size; px++ {
			var bgHits, fgHits float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					fx := float64(px) + (float64(sx)+0.5)/float64(ss)
					fy := float64(py) + (float64(sy)+0.5)/float64(ss)
					if sp.opaque {
						bgHits++
					} else if inRoundRect(fx, fy, float64(sp.size), sp.radius) {
						bgHits++
					} else {
						continue // 圆角外的透明区
					}
					if polyInside(plane, fx/float64(sp.size), fy/float64(sp.size)) {
						fgHits++
					}
				}
			}
			if bgHits == 0 {
				continue // 完全在圆角外 → 保持透明
			}
			// 背景渐变：#1e3a8a → #3b82f6（与控制台头部同色系）
			t := float64(py) / float64(sp.size-1)
			br := lerp(0x1e, 0x3b, t)
			bg := lerp(0x3a, 0x82, t)
			bb := lerp(0x8a, 0xf6, t)
			// 纸飞机按覆盖率混成白色
			f := fgHits * inv
			r := float64(br)*(1-f) + 255*f
			g := float64(bg)*(1-f) + 255*f
			b := float64(bb)*(1-f) + 255*f
			img.SetRGBA(px, py, color.RGBA{
				R: uint8(math.Round(r)),
				G: uint8(math.Round(g)),
				B: uint8(math.Round(b)),
				A: uint8(math.Round(bgHits * inv * 255)),
			})
		}
	}
	return img
}

func main() {
	// 输出目录：仓库的 html/（与 main.go 的 //go:embed 同级）
	outDir := "html"
	if len(os.Args) > 1 {
		outDir = os.Args[1]
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "创建输出目录失败: %v\n", err)
		os.Exit(1)
	}

	specs := []spec{
		// 通用图标：圆角方块 + 大飞机，192 是 manifest 必填尺寸。
		{name: "icon-192.png", size: 192, radius: 42, plane: 0.72},
		{name: "icon-512.png", size: 512, radius: 112, plane: 0.72},
		// maskable：满幅铺底，且图形收进中央 80% 安全区（按对角线算）。
		{name: "icon-maskable-512.png", size: 512, radius: 0, plane: 0.60, opaque: true},
		// iOS 主屏图标：系统自己切圆角，所以这里给满幅方图。
		{name: "apple-touch-icon.png", size: 180, radius: 0, plane: 0.72, opaque: true},
		// 浏览器标签页图标：尺寸小，图形占比拉高一点才看得清。
		{name: "favicon-32.png", size: 32, radius: 7, plane: 0.80},
	}

	for _, sp := range specs {
		img := draw(sp)
		path := filepath.Join(outDir, sp.name)
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "创建 %s 失败: %v\n", path, err)
			os.Exit(1)
		}
		if err := png.Encode(f, img); err != nil {
			f.Close()
			fmt.Fprintf(os.Stderr, "编码 %s 失败: %v\n", path, err)
			os.Exit(1)
		}
		if err := f.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "关闭 %s 失败: %v\n", path, err)
			os.Exit(1)
		}
		fmt.Printf("✓ %s (%dx%d)\n", path, sp.size, sp.size)
	}
}
