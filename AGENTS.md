# 项目开发约定

本文件是项目的主要开发指引。项目文档、代码注释和新增日志使用中文；根目录只维护一个 `README.md`。

## 项目结构

Collector 从 NATS JetStream 工作队列消费 BSON，批量写入 MongoDB，支持时序集合。

- `main.go`：初始化依赖、处理 SIGINT/SIGTERM、等待关闭完成。
- `bootstrap/`：读取 `config/values.yml`，初始化 NATS、KV 和 MongoDB。
- `app/`：连续监听 KV、管理收集器、批量写入、消息确认和状态查询。
- `transfer/`：创建流、发布 BSON、查询状态和删除流配置的 SDK。
- `e2e/`：显式启用的真实 NATS/MongoDB 集成测试。
- `.github/compose/`：测试基础设施。

## 开发与验证

```bash
go run .
go build ./...
go test ./...
CGO_ENABLED=1 go test -race ./...
go vet ./...
```

运行服务前准备 `config/values.yml`，不要覆盖已有配置。`.env` 仅供 E2E 测试加载。

集成测试需要设置 `NATS_HOSTS`、`NATS_TOKEN` 和 `MONGO_URL`，使用独立测试服务：

```bash
CGO_ENABLED=1 go test -race -tags=e2e ./e2e -count=1 -timeout=2m
```

集成测试自行启动应用实例，以随机前缀创建、清理命名空间和数据库；不复用业务命名空间或业务数据库。普通 `go test ./...` 不连接外部服务。

## 修改要求

- 不输出或提交 `.env`、`config/values.yml` 中的凭据。
- 修改本地文件使用补丁；保留与任务无关的用户改动。
- JSON 使用标准库 `encoding/json`，BSON 使用 MongoDB 驱动；不依赖 Go 运行时私有结构。
- 消息处理采用至少一次投递。只确认明确写入成功的消息；结果不确定时保留并退避重试。
- 不将所有唯一键冲突直接视为幂等成功；不静默终止或丢弃非法数据。
- 关闭流程必须等待消费回调、在途写入和最后刷新结束后再释放连接。
- 使用单个 KV 监听处理初始快照和增量事件，不以本地时钟过滤事件。
- 可靠性和并发修改应补充行为回归测试，并运行竞态检测与静态检查。
- 修改 SDK 的成功语义或同步行为时，同步更新 README 中的迁移说明。

## 依赖与架构

项目使用 Go Modules，最低 Go 版本以 `go.mod` 为准。没有 Wire 代码生成或 gocron 调度；收集器使用 JetStream 拉取消费回调和定时刷新。
