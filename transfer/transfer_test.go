package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestStreamName 测试流名称生成。
//
// 验证 StreamName 方法正确生成 {namespace}_{key} 格式的流名称。
func TestStreamName(t *testing.T) {
	x := &Transfer{Namespace: "test"}

	tests := []struct {
		key  string
		want string
	}{
		{"metrics", "test_metrics"},
		{"events", "test_events"},
		{"logs", "test_logs"},
	}

	for _, tt := range tests {
		if got := x.StreamName(tt.key); got != tt.want {
			t.Errorf("StreamName(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}

type testPublisher struct {
	jetstream.JetStream
	publish func(context.Context, string, []byte) (*jetstream.PubAck, error)
}

func (p *testPublisher) Publish(ctx context.Context, subject string, payload []byte, _ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	return p.publish(ctx, subject, payload)
}

func TestSendReturnsServerFailure(t *testing.T) {
	want := errors.New("服务端拒绝发布")
	x := &Transfer{Namespace: "test", Js: &testPublisher{publish: func(ctx context.Context, subject string, data []byte) (*jetstream.PubAck, error) {
		require.Equal(t, "test.metrics", subject)
		require.NoError(t, bson.Raw(data).Validate())
		_, hasDeadline := ctx.Deadline()
		require.True(t, hasDeadline)
		return nil, want
	}}}
	require.ErrorIs(t, x.Send("metrics", bson.M{"value": 1}), want)
}

func TestSendContextPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	x := &Transfer{Js: &testPublisher{publish: func(got context.Context, _ string, _ []byte) (*jetstream.PubAck, error) {
		require.Same(t, ctx, got)
		return nil, got.Err()
	}}}
	require.ErrorIs(t, x.SendContext(ctx, "metrics", bson.M{"value": 1}), context.Canceled)
}

// TestSubName 测试订阅主题名称生成。
//
// 验证 SubName 方法正确生成 {namespace}.{key} 格式的主题名称。
func TestSubName(t *testing.T) {
	x := &Transfer{Namespace: "test"}

	tests := []struct {
		key  string
		want string
	}{
		{"metrics", "test.metrics"},
		{"events", "test.events"},
		{"logs", "test.logs"},
	}

	for _, tt := range tests {
		if got := x.SubName(tt.key); got != tt.want {
			t.Errorf("SubName(%q) = %q, want %q", tt.key, got, tt.want)
		}
	}
}
