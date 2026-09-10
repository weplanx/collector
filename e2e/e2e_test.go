//go:build e2e

package e2e_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/kainonly/collector/v3/app"
	"github.com/kainonly/collector/v3/bootstrap"
	"github.com/kainonly/collector/v3/common"
	"github.com/kainonly/collector/v3/transfer"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestCollectorEndToEnd 使用独立命名空间和数据库验证真实落库，不依赖用例执行顺序。
func TestCollectorEndToEnd(t *testing.T) {
	_ = godotenv.Load("../.env")
	require.NotEmpty(t, os.Getenv("NATS_HOSTS"), "E2E 测试需要 NATS_HOSTS")
	require.NotEmpty(t, os.Getenv("MONGO_URL"), "E2E 测试需要 MONGO_URL")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	nc, err := nats.Connect(os.Getenv("NATS_HOSTS"), nats.Token(os.Getenv("NATS_TOKEN")), nats.Timeout(5*time.Second))
	require.NoError(t, err)
	defer nc.Close()
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	namespace := "collector_e2e_" + bson.NewObjectID().Hex()
	v := &common.Values{Namespace: namespace, BatchSize: 20, FlushInterval: 100 * time.Millisecond, MongoUrl: os.Getenv("MONGO_URL"), MongoDatabase: namespace}
	mc, err := bootstrap.UseMongo(v)
	require.NoError(t, err)
	defer mc.Disconnect(context.Background())
	require.NoError(t, mc.Ping(ctx, nil))
	db := bootstrap.UseDatabase(v, mc)
	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: namespace, History: 3})
	require.NoError(t, err)
	x := app.New(v, nc, js, kv, db)
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	started := false
	keys := []string{"snapshot", "live"}
	defer func() {
		stop()
		if started {
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("运行循环失败: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Error("运行循环未退出")
			}
		}
		x.Close()
		cleanup, cancelCleanup := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancelCleanup()
		// 仅清理本用例生成的随机命名空间和数据库。
		for _, key := range keys {
			err := js.DeleteStream(cleanup, namespace+"_"+key)
			if err != nil && !errors.Is(err, jetstream.ErrStreamNotFound) {
				t.Errorf("清理流失败: %v", err)
			}
		}
		if err := js.DeleteKeyValue(cleanup, namespace); err != nil {
			t.Errorf("清理 KV 失败: %v", err)
		}
		if err := db.Drop(cleanup); err != nil {
			t.Errorf("清理数据库失败: %v", err)
		}
	}()
	sdk, err := transfer.New(ctx, namespace, nc)
	require.NoError(t, err)
	for _, key := range keys {
		require.NoError(t, db.CreateCollection(ctx, key, options.CreateCollection().SetTimeSeriesOptions(options.TimeSeries().SetTimeField("ts"))))
	}
	// 启动前注册验证快照，启动后注册验证连续监听。
	require.NoError(t, sdk.Add(ctx, transfer.Option{Key: keys[0]}))
	require.NoError(t, x.States())
	require.NoError(t, nc.Flush())
	started = true
	go func() { done <- x.Run(runCtx) }()
	require.NoError(t, sdk.Add(ctx, transfer.Option{Key: keys[1]}))
	for _, key := range keys {
		for i := 0; i < 25; i++ {
			require.NoError(t, sdk.SendContext(ctx, key, bson.M{"ts": time.Now(), "value": i}))
		}
		require.Eventually(t, func() bool {
			count, err := db.Collection(key).CountDocuments(ctx, bson.D{})
			return err == nil && count == 25
		}, 15*time.Second, 50*time.Millisecond, "数据没有完整落库: %s", key)
		option, err := sdk.Get(ctx, key)
		require.NoError(t, err)
		require.Equal(t, key, option.Key)
	}
	// 等待队列清空，以检查写入后确认消息的完整链路。
	for _, key := range keys {
		require.Eventually(t, func() bool {
			stream, err := js.Stream(ctx, namespace+"_"+key)
			if err != nil {
				return false
			}
			info, err := stream.Info(ctx)
			return err == nil && info.State.Msgs == 0
		}, 10*time.Second, 50*time.Millisecond)
	}
	// 真实服务端拒绝没有匹配流的发布时，SDK 必须返回错误。
	require.Error(t, sdk.SendContext(ctx, "missing", bson.M{"ts": time.Now()}))
	// 时序集合拒绝缺少时间字段的文档；其余文档应正常写入，不能随失败项重复插入。
	require.NoError(t, sdk.SendContext(ctx, "live", bson.M{"value": "缺少时间字段"}))
	for i := 0; i < 2; i++ {
		require.NoError(t, sdk.SendContext(ctx, "live", bson.M{"ts": time.Now(), "value": i + 25}))
	}
	require.Eventually(t, func() bool {
		consumer, err := js.Consumer(ctx, namespace+"_live", "default")
		if err != nil {
			return false
		}
		info, err := consumer.Info(ctx)
		return err == nil && info.NumRedelivered > 0 && info.NumAckPending == 1
	}, 10*time.Second, 50*time.Millisecond, "失败文档未被单独保留重试")
	count, err := db.Collection("live").CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	require.EqualValues(t, 27, count, "部分失败导致成功文档丢失或重复")
	for _, key := range keys {
		require.NoError(t, sdk.Remove(ctx, key))
		require.Eventually(t, func() bool {
			_, err := js.Stream(ctx, namespace+"_"+key)
			return errors.Is(err, jetstream.ErrStreamNotFound)
		}, 10*time.Second, 50*time.Millisecond)
	}
}
