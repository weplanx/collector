package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kainonly/collector/v3/common"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// testMessage 只实现测试所需的消息行为，意外调用其他方法会直接失败。
type testMessage struct {
	jetstream.Msg
	data       []byte
	acked      bool
	delay      time.Duration
	deliveries uint64
}

func (m *testMessage) Data() []byte                           { return m.data }
func (m *testMessage) Ack() error                             { m.acked = true; return nil }
func (m *testMessage) NakWithDelay(delay time.Duration) error { m.delay = delay; return nil }
func (m *testMessage) Metadata() (*jetstream.MsgMetadata, error) {
	return &jetstream.MsgMetadata{NumDelivered: m.deliveries}, nil
}

func newTestCollector() *Collector {
	return NewCollector(New(&common.Values{BatchSize: 10, FlushInterval: time.Hour}, nil, nil, nil, nil), Option{Key: "测试"})
}

func newMessage(t *testing.T) *testMessage {
	t.Helper()
	data, err := bson.Marshal(bson.M{"value": 1})
	require.NoError(t, err)
	return &testMessage{data: data, deliveries: 1}
}

func TestFlushPartialFailure(t *testing.T) {
	c := newTestCollector()
	first, failed, last := newMessage(t), newMessage(t), newMessage(t)
	c.buffer = []jetstream.Msg{first, failed, last}
	c.insert = func(_ context.Context, docs []any) error {
		require.Len(t, docs, 3)
		return mongo.BulkWriteException{WriteErrors: []mongo.BulkWriteError{{WriteError: mongo.WriteError{Index: 1, Code: 121}}}}
	}
	c.flush()
	require.True(t, first.acked)
	require.True(t, last.acked)
	require.False(t, failed.acked)
	require.Equal(t, time.Second, failed.delay)
}

func TestFlushUnknownResult(t *testing.T) {
	for _, err := range []error{
		errors.New("连接中断"),
		mongo.BulkWriteException{WriteConcernError: &mongo.WriteConcernError{Code: 64}},
		mongo.BulkWriteException{WriteErrors: []mongo.BulkWriteError{{WriteError: mongo.WriteError{Index: 99}}}},
	} {
		c := newTestCollector()
		msg := newMessage(t)
		c.buffer = []jetstream.Msg{msg}
		c.insert = func(context.Context, []any) error { return err }
		c.flush()
		require.False(t, msg.acked)
		require.Positive(t, msg.delay)
	}
}

func TestFlushInvalidBSONDoesNotBlockValidMessage(t *testing.T) {
	c := newTestCollector()
	invalid := &testMessage{data: []byte("无效 BSON"), deliveries: 100}
	valid := newMessage(t)
	c.buffer = []jetstream.Msg{invalid, valid}
	c.insert = func(_ context.Context, docs []any) error { require.Len(t, docs, 1); return nil }
	c.flush()
	require.False(t, invalid.acked)
	require.Equal(t, time.Minute, invalid.delay)
	require.True(t, valid.acked)
}

type testConsumeContext struct {
	jetstream.ConsumeContext
	closed   chan struct{}
	stopOnce sync.Once
}

func (c *testConsumeContext) Stop()                   { c.stopOnce.Do(func() { close(c.closed) }) }
func (c *testConsumeContext) Closed() <-chan struct{} { return c.closed }

type testConsumer struct {
	jetstream.Consumer
	cc *testConsumeContext
}

func (c *testConsumer) Consume(jetstream.MessageHandler, ...jetstream.PullConsumeOpt) (jetstream.ConsumeContext, error) {
	return c.cc, nil
}

func TestStopWaitsForFinalFlushAndIsIdempotent(t *testing.T) {
	c := newTestCollector()
	msg := newMessage(t)
	c.buffer = []jetstream.Msg{msg}
	entered, release := make(chan struct{}), make(chan struct{})
	c.insert = func(context.Context, []any) error { close(entered); <-release; return nil }
	require.NoError(t, c.Start("测试", &testConsumer{cc: &testConsumeContext{closed: make(chan struct{})}}))
	stopped := make(chan struct{})
	go func() { c.Stop(); c.Stop(); close(stopped) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("没有执行最后刷新")
	}
	select {
	case <-stopped:
		t.Fatal("写入结束前 Stop 已返回")
	default:
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop 没有结束")
	}
	require.True(t, msg.acked)
}

func TestStopWaitsForActiveFlush(t *testing.T) {
	c := newTestCollector()
	msg := newMessage(t)
	c.buffer = []jetstream.Msg{msg}
	entered, release := make(chan struct{}), make(chan struct{})
	c.insert = func(context.Context, []any) error { close(entered); <-release; return nil }
	require.NoError(t, c.Start("测试", &testConsumer{cc: &testConsumeContext{closed: make(chan struct{})}}))
	go c.flush()
	<-entered
	stopped := make(chan struct{})
	go func() { c.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("在途写入结束前 Stop 已返回")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop 没有结束")
	}
	require.True(t, msg.acked)
}

func TestInvalidBatchConfiguration(t *testing.T) {
	c := newTestCollector()
	c.app.V.FlushInterval = 0
	require.Error(t, c.Start("测试", nil))
	c.Stop()
}
