package ds

import (
	"github.com/go-playground/validator/v10"
	"github.com/nats-io/nats.go"
	cls "github.com/tencentcloud/tencentcloud-cls-sdk-go"
	"github.com/vmihailenco/msgpack/v5"
	"go.uber.org/zap"
	"time"
)

type CLS struct {
	Client *cls.AsyncProducerClient
	Logger *zap.Logger
}

func NewCLS(option map[string]interface{}, logger *zap.Logger) (_ DataSource, err error) {
	x := new(CLS)
	producerConfig := cls.GetDefaultAsyncProducerClientConfig()
	producerConfig.AccessKeyID = option["secret_id"].(string)
	producerConfig.AccessKeySecret = option["secret_key"].(string)
	producerConfig.Endpoint = option["endpoint"].(string)
	if x.Client, err = cls.NewAsyncProducerClient(producerConfig); err != nil {
		return
	}
	x.Client.Start()
	x.Logger = logger
	return x, nil
}

type CLSDto struct {
	TopicId string            `msgpack:"topic_id" validate:"required"`
	Record  map[string]string `msgpack:"record" validate:"required"`
	Time    time.Time         `msgpack:"time" validate:"required"`
}

func (x *CLS) Push(msg *nats.Msg) (err error) {
	var data CLSDto
	if err = msgpack.Unmarshal(msg.Data, &data); err != nil {
		return
	}
	if err = validator.New().Struct(&data); err != nil {
		msg.Term()
		return
	}
	x.Logger.Debug("解码成功",
		zap.String("subject", msg.Subject),
		zap.Any("data", data),
		zap.Error(err),
	)
	clog := cls.NewCLSLog(data.Time.Unix(), data.Record)
	reply := &CLSReply{Logger: x.Logger, Msg: msg}
	if err = x.Client.SendLog(data.TopicId, clog, reply); err != nil {
		return
	}
	return
}

type CLSReply struct {
	Msg    *nats.Msg
	Logger *zap.Logger
}

func (x *CLSReply) Success(result *cls.Result) {
	x.Msg.Ack()
	x.Logger.Debug("日志写入成功",
		zap.Any("attempts", result.GetReservedAttempts()),
	)
}

func (x *CLSReply) Fail(result *cls.Result) {
	x.Msg.Nak()
	x.Logger.Debug("日志写入失败",
		zap.String("request_id", result.GetRequestId()),
		zap.String("code", result.GetErrorCode()),
		zap.String("msg", result.GetErrorMessage()),
	)
}