// Package main 是收集器服务的入口点。
//
// 收集器是一个队列驱动的 MongoDB 时序数据采集服务，
// 从 NATS JetStream 工作队列流中消费 BSON 数据，
// 按固定调度批量写入 MongoDB。
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kainonly/collector/v3/app"
	"github.com/kainonly/collector/v3/bootstrap"
	"github.com/kainonly/collector/v3/common"
)

func main() {
	if err := run(); err != nil {
		common.Log.Error(err.Error())
		os.Exit(1)
	}
}

// run 在返回前完成刷新并释放连接，使运行循环异常也能触发有序退出。
func run() error {
	// 初始化日志记录器
	// 根据 MODE 环境变量选择开发模式或生产模式
	var err error
	if common.Log, err = bootstrap.SetZap(); err != nil {
		panic(err)
	}

	// 从 config/values.yml 加载配置
	values, err := bootstrap.LoadStaticValues()
	if err != nil {
		panic(err)
	}

	// 建立 NATS 连接，支持自动重连
	nc, err := bootstrap.UseNats(values)
	if err != nil {
		panic(err)
	}
	defer func() {
		// 确保最后一批确认消息已发送，再关闭 NATS 连接。
		if err := nc.FlushTimeout(5 * time.Second); err != nil {
			common.Log.Error("关闭前发送消息确认失败: " + err.Error())
		}
		nc.Close()
	}()

	// 创建 JetStream 上下文，用于流和消费者操作
	js, err := bootstrap.UseJetStream(nc)
	if err != nil {
		panic(err)
	}

	// 获取或创建命名空间 KV 存储桶，用于存储流配置
	kv, err := bootstrap.UseKeyValue(values, js)
	if err != nil {
		panic(err)
	}

	// 建立 MongoDB 连接
	mc, err := bootstrap.UseMongo(values)
	if err != nil {
		panic(err)
	}
	defer mc.Disconnect(context.Background())

	// 获取数据库句柄，使用 majority 写入关注确保数据一致性
	db := bootstrap.UseDatabase(values, mc)

	// 创建可取消的上下文，用于优雅关闭
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// 初始化应用实例
	x := app.New(values, nc, js, kv, db)

	// 注册状态查询端点，允许外部查询收集器状态
	if err = x.States(); err != nil {
		panic(err)
	}

	// 主运行循环加载配置并监听变更，退出后先关闭收集器再释放连接。
	defer x.Close()
	err = x.Run(ctx)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
