package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kainonly/collector/v3/common"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
)

type testWatcher struct {
	jetstream.KeyWatcher
	updates chan jetstream.KeyValueEntry
	stopped bool
}

func (w *testWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }
func (w *testWatcher) Stop() error                             { w.stopped = true; return nil }

type testKV struct {
	jetstream.KeyValue
	watcher *testWatcher
}

func (kv *testKV) WatchAll(context.Context, ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return kv.watcher, nil
}

type testEntry struct {
	jetstream.KeyValueEntry
	key  string
	data []byte
}

func (e testEntry) Key() string                     { return e.key }
func (e testEntry) Value() []byte                   { return e.data }
func (e testEntry) Operation() jetstream.KeyValueOp { return jetstream.KeyValuePut }

// 返回旧时间，确保快照和变更都不会再被墙上时钟过滤。
func (e testEntry) Created() time.Time { return time.Unix(1, 0) }

type testJetStream struct {
	jetstream.JetStream
	subscribed chan string
}

func (js *testJetStream) Consumer(_ context.Context, stream, _ string) (jetstream.Consumer, error) {
	js.subscribed <- stream
	return &testConsumer{cc: &testConsumeContext{closed: make(chan struct{})}}, nil
}

func TestWatchLoadsSnapshotAndUpdates(t *testing.T) {
	w := &testWatcher{updates: make(chan jetstream.KeyValueEntry, 8)}
	js := &testJetStream{subscribed: make(chan string, 4)}
	x := New(&common.Values{Namespace: "test", BatchSize: 10, FlushInterval: time.Hour}, nil, js, &testKV{watcher: w}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- x.Run(ctx) }()
	for _, key := range []string{"初始", "新增"} {
		data, err := json.Marshal(Option{Key: key})
		require.NoError(t, err)
		w.updates <- testEntry{key: key, data: data}
		w.updates <- nil
	}
	w.updates <- testEntry{key: "无效", data: []byte("{")}
	data, err := json.Marshal(Option{Key: "最后"})
	require.NoError(t, err)
	w.updates <- testEntry{key: "最后", data: data}
	for _, want := range []string{"test_初始", "test_新增", "test_最后"} {
		select {
		case got := <-js.subscribed:
			require.Equal(t, want, got)
		case <-time.After(time.Second):
			t.Fatal("配置事件丢失")
		}
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("监听未响应取消")
	}
	require.True(t, w.stopped)
	x.Close()
	x.Close()
	require.Error(t, x.Subscribe(Option{Key: "关闭后"}))
}
