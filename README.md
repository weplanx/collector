# Collector

轻量级 BSON 数据采集服务：从 NATS JetStream 工作队列消费消息，批量写入 MongoDB，支持时序集合和多个数据流。

## 启动

需要 Go（最低版本见 `go.mod`）、启用 JetStream 的 NATS，以及 MongoDB。使用时序集合时需要 MongoDB 5.0 或以上版本。

首次运行时准备配置，已有配置请直接使用：

```bash
cp config/values.example.yml config/values.yml
go run .
```

服务仅读取 `config/values.yml`；`.env` 用于集成测试。

```yaml
mode: debug
namespace: alpha
description: 数据采集配置
batch_size: 1000
flush_interval: 5s
nats_hosts:
  - nats://127.0.0.1:4222
nats_token: your-token
mongo_url: mongodb://localhost:27017
mongo_database: example
```

`batch_size` 和 `flush_interval` 必须大于零。达到批量阈值或刷新间隔后触发写入。日志模式目前由环境变量 `MODE` 决定，例如 `MODE=release go run .`；YAML 的 `mode` 字段不控制日志初始化。

## 数据流与可靠性

生产者通过 SDK 创建流 `{namespace}_{key}`、主题 `{namespace}.{key}` 和消费者 `default`，再将流配置写入同名命名空间的 KV 桶。服务通过同一个 KV 监听加载初始快照并接收后续变更，使用 JetStream 拉取消费回调累积 BSON 数据。

- MongoDB 使用无序批量插入及 `majority` 写入关注；部分失败时确认成功项，仅重试失败项。
- 非法 BSON 单独保留重试，不阻断同批次的合法消息。失败重投按 1、2、4 秒递增，最长间隔 60 秒；持续失败需要检查日志和修正数据或目标集合约束。
- 网络中断、写入关注错误等情况下可能无法确定落库结果，此时保留消息重试。NATS 和 MongoDB 之间没有跨系统事务，提供至少一次投递，仍可能重复写入。
- 唯一键冲突不会直接被视为成功；时序集合也不能依赖 `_id` 唯一性实现去重。需要严格去重的业务应另行设计幂等存储或消费处理。
- SIGINT/SIGTERM 触发有序关闭，等待消费回调、在途写入和最后刷新后再断开连接。容器的退出宽限期应覆盖写入超时和最后刷新，建议至少 45 秒。
- 无法建立流订阅或配置监听意外结束时，主进程返回错误并退出，便于进程管理器重启。

MongoDB 时序集合需要提前创建，文档中的时间字段必须与集合的 `timeField` 一致。服务不会自动创建时序集合；不存在的目标集合可能由 MongoDB 创建为普通集合。

## Transfer SDK

KV 桶须由收集器或部署流程预先创建，生产者和收集器的命名空间必须一致。

```go
package main

import (
    "context"
    "log"
    "time"

    "github.com/kainonly/collector/v3/transfer"
    "github.com/nats-io/nats.go"
    "go.mongodb.org/mongo-driver/v2/bson"
)

func main() {
    ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()
    nc, err := nats.Connect("nats://127.0.0.1:4222", nats.Token("your-token"))
    if err != nil { log.Fatal(err) }
    defer nc.Close()
    sdk, err := transfer.New(ctx, "alpha", nc)
    if err != nil { log.Fatal(err) }
    if err := sdk.Add(ctx, transfer.Option{Key: "metrics", Collection: "metrics"}); err != nil {
        log.Fatal(err)
    }
    if err := sdk.SendContext(ctx, "metrics", bson.M{"ts": time.Now(), "cpu": 0.42}); err != nil {
        log.Fatal(err)
    }
}
```

`Send(key, data)` 等待 JetStream 服务端确认，默认超时 15 秒；`SendContext` 允许调用方控制取消和超时。成功代表服务端已确认发布，不代表已写入 MongoDB。

迁移注意：`Send` 从异步提交改为同步等待确认，函数签名保持不变。原先等待 `PublishAsyncComplete()` 的代码可以移除；高吞吐场景可采用受控并发调用 `SendContext`，并检查每次调用的错误。

`Get(ctx, key)` 查询配置和当前实例的缓冲区大小。`BufferSize == 0` 不代表整个队列已落库，也不包含正在执行的批次；多实例部署下该状态不是全局汇总。

`Remove(ctx, key)` 删除 KV 配置，服务随后停止对应收集器并删除流。该操作会删除流内尚未消费或仍待重试的消息，调用前应确认允许清理这些数据。

## 测试

```bash
go build ./...
go test ./...
CGO_ENABLED=1 go test -race ./...
go vet ./...
```

普通测试不连接外部服务。集成测试需要显式添加 `e2e` 构建标签，测试会启动应用实例，创建随机命名空间和数据库，验证时序数据落库、消息确认、发布失败和流移除，结束时清理自己的资源。

可以使用 `.github/compose/docker-compose.yml` 启动本地测试服务：

```bash
docker compose -f .github/compose/docker-compose.yml up -d
NATS_HOSTS=nats://127.0.0.1:4222 NATS_TOKEN=s3cr3t \
MONGO_URL=mongodb://root:password@127.0.0.1:27017 \
CGO_ENABLED=1 go test -race -tags=e2e ./e2e -count=1 -timeout=2m
```

也可以配置 `.env` 中的 `NATS_HOSTS`、`NATS_TOKEN`、`MONGO_URL`。测试不使用业务 `NATS_NAMESPACE` 或 `MONGO_DATABASE`，连接账户需要有权创建及清理测试资源。

## 部署与开发

构建二进制后可使用仓库中的 Dockerfile 打包，配置目录挂载到 `/app/config`。保证进程能够收到 SIGTERM，并设置足够的退出宽限期。

开发规范和目录说明见 [AGENTS.md](AGENTS.md)。项目仅维护这份中文 README，源码注释与新增日志使用中文。

## 许可证

[BSD-3-Clause](LICENSE)
