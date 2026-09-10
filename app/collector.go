package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kainonly/collector/v3/common"
	"github.com/nats-io/nats.go/jetstream"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.uber.org/zap"
)

// Collector 负责单个流的消息消费和批量写入。
//
// 工作机制：
//  1. 从 JetStream 消费者接收消息
//  2. 将消息累积到缓冲区
//  3. 触发刷新的条件（满足任一）：
//     - 缓冲区达到 BatchSize
//     - 定时器到达 FlushInterval
//     - 收到停止信号
//  4. 批量写入 MongoDB
//  5. 成功则 ACK，失败则 NAK 以便重试
type Collector struct {
	// app 父级 App 实例，提供配置和数据库连接
	app *App
	// option 流配置
	option Option
	// cc JetStream 消费上下文，用于停止消费
	cc jetstream.ConsumeContext

	// mu 保护 buffer 的互斥锁
	mu sync.Mutex
	// buffer 消息缓冲区
	buffer []jetstream.Msg
	// stopCh 停止信号通道
	stopCh chan struct{}
	// done 在最后一次刷新完成后关闭；stopOnce 保证重复停止安全。
	done      chan struct{}
	stopOnce  sync.Once
	flushMu   sync.Mutex
	lifecycle sync.Mutex
	started   bool
	stopped   bool
	// insert 允许在测试中模拟部分成功、超时和关闭期间的写入。
	insert func(context.Context, []any) error
}

// NewCollector 创建新的 Collector 实例。
//
// 缓冲区预分配 BatchSize 容量以减少内存分配。
func NewCollector(app *App, option Option) *Collector {
	c := &Collector{
		app:    app,
		option: option,
		buffer: make([]jetstream.Msg, 0, max(0, app.V.BatchSize)),
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
	c.insert = func(ctx context.Context, documents []any) error {
		name := option.Collection
		if name == "" {
			name = option.Key
		}
		_, err := app.Db.Collection(name).InsertMany(ctx, documents, options.InsertMany().SetOrdered(false))
		return err
	}
	return c
}

// Start 开始从 JetStream 消费者接收消息。
//
// 启动两个并发任务：
//   - 消息接收回调：将消息推入缓冲区
//   - 定时刷新循环：按 FlushInterval 定期刷新
func (c *Collector) Start(key string, consumer jetstream.Consumer) (err error) {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	if c.started || c.stopped {
		return errors.New("收集器已启动或停止")
	}
	if c.app.V.BatchSize <= 0 || c.app.V.FlushInterval <= 0 {
		return fmt.Errorf("batch_size 和 flush_interval 必须大于零")
	}
	// Consume 通过持续拉取消费并调用消息处理函数。
	if c.cc, err = consumer.Consume(func(msg jetstream.Msg) {
		c.push(msg)
	}); err != nil {
		return
	}
	// 启动定时刷新循环
	c.started = true
	go c.flushLoop()
	return
}

// Stop 停止消费并执行最后一次刷新。
//
// 调用此方法后：
//  1. 停止从 JetStream 接收新消息
//  2. 通知 flushLoop 退出
//  3. flushLoop 在退出前会刷新缓冲区中的剩余消息
func (c *Collector) Stop() {
	c.stopOnce.Do(func() {
		c.lifecycle.Lock()
		defer c.lifecycle.Unlock()
		c.stopped = true
		if c.cc != nil {
			c.cc.Stop()
			<-c.cc.Closed()
		}
		close(c.stopCh)
		if !c.started {
			c.flush()
			close(c.done)
		}
	})
	<-c.done
}

// BufferSize 返回当前缓冲区中的消息数量。
//
// 此方法是线程安全的，用于状态查询接口。
func (c *Collector) BufferSize() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.buffer)
}

// push 将消息添加到缓冲区。
//
// 如果缓冲区达到 BatchSize，立即触发刷新。
// 此方法由 JetStream 消费回调调用。
func (c *Collector) push(msg jetstream.Msg) {
	c.mu.Lock()
	c.buffer = append(c.buffer, msg)
	shouldFlush := len(c.buffer) >= c.app.V.BatchSize
	c.mu.Unlock()

	if shouldFlush {
		c.flush()
	}
}

// flushLoop 定时刷新循环。
//
// 按 FlushInterval 定期触发刷新，确保消息不会
// 在缓冲区中停留过久。
//
// 收到停止信号时，执行最后一次刷新再退出，通知 Stop 可以继续释放连接。
func (c *Collector) flushLoop() {
	defer close(c.done)
	ticker := time.NewTicker(c.app.V.FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.flush()
		case <-c.stopCh:
			c.flush() // 停止前最后刷新一次
			return
		}
	}
}

// flush 将缓冲区中的消息批量写入 MongoDB。
//
// 执行流程：
//  1. 获取锁，取出所有缓冲消息，重置缓冲区
//  2. 释放锁（允许新消息继续入队）
//  3. 校验 BSON，单独延迟重试非法消息
//  4. 无序批量插入，确认成功项，仅延迟重试失败项
//  5. 网络错误或写入关注错误导致结果不确定时，保留消息重试
//
// 消息数据直接以原始 BSON 格式写入，无需反序列化。
func (c *Collector) flush() {
	// 串行化定时刷新与达到批量阈值的刷新，避免关闭时遗漏正在写入的批次。
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	// 快速获取并清空缓冲区
	c.mu.Lock()
	if len(c.buffer) == 0 {
		c.mu.Unlock()
		return
	}
	msgs := c.buffer
	c.buffer = make([]jetstream.Msg, 0, c.app.V.BatchSize)
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// 提取消息数据（原始 BSON 字节）
	documents := make([]any, 0, len(msgs))
	valid := make([]jetstream.Msg, 0, len(msgs))
	for _, msg := range msgs {
		if err := bson.Raw(msg.Data()).Validate(); err != nil {
			common.Log.Error("BSON 数据无效，保留消息并延迟重试", zap.String("key", c.option.Key), zap.Error(err))
			c.retry(msg)
			continue
		}
		documents = append(documents, bson.Raw(msg.Data()))
		valid = append(valid, msg)
	}
	if len(valid) == 0 {
		return
	}
	err := c.insert(ctx, documents)
	if err != nil {
		common.Log.Error("刷新失败",
			zap.String("key", c.option.Key),
			zap.Int("count", len(msgs)),
			zap.Error(err),
		)
	}
	failed, known := failedWrites(err, len(valid))
	for i, msg := range valid {
		if !known || failed[i] {
			c.retry(msg)
			continue
		}
		if ackErr := msg.Ack(); ackErr != nil {
			common.Log.Error("确认消息失败", zap.String("key", c.option.Key), zap.Error(ackErr))
		}
	}
	if err == nil {
		common.Log.Info("刷新成功",
			zap.String("key", c.option.Key),
			zap.Int("count", len(msgs)),
		)
	}
}

// failedWrites 仅在无序写入结果明确时确认未出错项，不将唯一键冲突误判为成功。
func failedWrites(err error, count int) (map[int]bool, bool) {
	failed := make(map[int]bool)
	if err == nil {
		return failed, true
	}
	var bulk mongo.BulkWriteException
	if !errors.As(err, &bulk) || bulk.WriteConcernError != nil || len(bulk.WriteErrors) == 0 {
		return nil, false
	}
	for _, item := range bulk.WriteErrors {
		if item.Index < 0 || item.Index >= count {
			return nil, false
		}
		failed[item.Index] = true
	}
	return failed, true
}

// retry 使用有上限的指数退避，非法消息继续保留在队列中等待人工修正。
func (c *Collector) retry(msg jetstream.Msg) {
	delay := time.Second
	if metadata, err := msg.Metadata(); err == nil {
		for attempt := uint64(1); attempt < metadata.NumDelivered && delay < time.Minute; attempt++ {
			delay *= 2
		}
	}
	if delay > time.Minute {
		delay = time.Minute
	}
	if err := msg.NakWithDelay(delay); err != nil {
		common.Log.Error("延迟重投失败", zap.String("key", c.option.Key), zap.Error(err))
	}
}
