package main

// xray-core 库的统一接入层：所有对 xray-core 包的直接依赖（core/serial/distro
// 等）都收敛在本文件，其余代码只使用这里暴露的类型与函数。
// 升级 xray-core 版本时只需关注本文件是否需要适配。

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"time"

	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/infra/conf/serial"
	_ "github.com/xtls/xray-core/main/distro/all" // 注册全部协议与功能模块
)

// setupXrayAssetLocation 让内嵌核心在指定目录查找 geoip.dat/geosite.dat。
func setupXrayAssetLocation(dir string) {
	os.Setenv("XRAY_LOCATION_ASSET", dir)
}

// xrayInstance 包装 core.Instance，对外不暴露 xray-core 的类型。
type xrayInstance struct {
	inst *core.Instance
}

// startXrayInstance 把 buildXrayConfig 生成的 JSON 配置构建成内存中的 xray
// 实例并启动（入站监听随即就绪）。用完后调用 Close() 释放。
func startXrayInstance(cfgJSON []byte) (*xrayInstance, error) {
	conf, err := serial.DecodeJSONConfig(bytes.NewReader(cfgJSON))
	if err != nil {
		return nil, fmt.Errorf("解析 xray 配置失败: %w", err)
	}
	coreCfg, err := conf.Build()
	if err != nil {
		return nil, fmt.Errorf("构建 xray 配置失败: %w", err)
	}
	inst, err := core.New(coreCfg)
	if err != nil {
		return nil, err
	}
	if err := inst.Start(); err != nil {
		inst.Close()
		return nil, err
	}
	return &xrayInstance{inst: inst}, nil
}

// Close 停止实例并释放全部资源（关闭监听端口与连接）。
func (x *xrayInstance) Close() error {
	if x == nil || x.inst == nil {
		return nil
	}
	return x.inst.Close()
}

// swapOutbound 热替换实例的 proxy 出站：监听端口与实例本身完全不动。
// 从新配置里取出 proxy 出站，摘除旧的、挂上新的；旧出站延迟 30 秒关闭，
// 让在途连接自然排空（浏览器等客户端零感知）。
func (x *xrayInstance) swapOutbound(cfgJSON []byte) error {
	conf, err := serial.DecodeJSONConfig(bytes.NewReader(cfgJSON))
	if err != nil {
		return err
	}
	coreCfg, err := conf.Build()
	if err != nil {
		return err
	}
	var newHandlerConfig *core.OutboundHandlerConfig
	for _, ob := range coreCfg.Outbound {
		if ob.Tag == "proxy" {
			newHandlerConfig = ob
			break
		}
	}
	if newHandlerConfig == nil {
		return fmt.Errorf("新配置缺少 proxy 出站")
	}

	obManager, ok := x.inst.GetFeature(outbound.ManagerType()).(outbound.Manager)
	if !ok {
		return fmt.Errorf("未找到出站管理器")
	}
	oldHandler := obManager.GetHandler("proxy")
	// 先摘除旧出站（仅从路由表移除，不断开旧连接），紧接着挂上新出站
	if err := obManager.RemoveHandler(context.Background(), "proxy"); err != nil {
		return err
	}
	if err := core.AddOutboundHandler(x.inst, newHandlerConfig); err != nil {
		return err
	}
	if oldHandler != nil {
		go func() {
			time.Sleep(30 * time.Second)
			oldHandler.Close()
		}()
	}
	return nil
}
