package mq

import (
	"elastic-collector/app/actions"
	"elastic-collector/app/schema"
	"elastic-collector/app/types"
	"github.com/elastic/go-elasticsearch/v8"
	"github.com/sirupsen/logrus"
	"github.com/streadway/amqp"
	"time"
)

type AmqpDrive struct {
	url             string
	client          *elasticsearch.Client
	schema          *schema.Schema
	conn            *amqp.Connection
	notifyConnClose chan *amqp.Error
	channel         map[string]*amqp.Channel
	channelDone     map[string]chan int
	notifyChanClose map[string]chan *amqp.Error
}

func NewAmqpDrive(url string, client *elasticsearch.Client, schema *schema.Schema) (session *AmqpDrive, err error) {
	session = new(AmqpDrive)
	session.url = url
	session.client = client
	session.schema = schema
	conn, err := amqp.Dial(url)
	if err != nil {
		return
	}
	session.conn = conn
	session.notifyConnClose = make(chan *amqp.Error)
	conn.NotifyClose(session.notifyConnClose)
	go session.listenConn()
	session.channel = make(map[string]*amqp.Channel)
	session.channelDone = make(map[string]chan int)
	session.notifyChanClose = make(map[string]chan *amqp.Error)
	return
}

func (c *AmqpDrive) listenConn() {
	select {
	case <-c.notifyConnClose:
		logrus.Error("AMQP connection has been disconnected")
		c.reconnected()
	}
}

func (c *AmqpDrive) reconnected() {
	count := 0
	for {
		time.Sleep(time.Second * 5)
		count++
		logrus.Info("Trying to reconnect:", count)
		conn, err := amqp.Dial(c.url)
		if err != nil {
			logrus.Error(err)
			continue
		}
		c.conn = conn
		c.notifyConnClose = make(chan *amqp.Error)
		conn.NotifyClose(c.notifyConnClose)
		go c.listenConn()
		logrus.Info("Attempt to reconnect successfully")
		break
	}
}

func (c *AmqpDrive) SetChannel(ID string) (err error) {
	c.channel[ID], err = c.conn.Channel()
	if err != nil {
		return
	}
	c.channelDone[ID] = make(chan int)
	c.notifyChanClose[ID] = make(chan *amqp.Error)
	c.channel[ID].NotifyClose(c.notifyChanClose[ID])
	go c.listenChannel(ID)
	return
}

func (c *AmqpDrive) listenChannel(ID string) {
	select {
	case <-c.notifyChanClose[ID]:
		logrus.Error("Channel connection is disconnected:", ID)
		c.refreshChannel(ID)
	case <-c.channelDone[ID]:
		break
	}
}

func (c *AmqpDrive) refreshChannel(ID string) {
	for {
		err := c.SetChannel(ID)
		if err != nil {
			continue
		}
		option, err := c.schema.Get(ID)
		if err != nil {
			continue
		}
		err = c.SetConsume(option)
		if err != nil {
			continue
		}
		logrus.Info("Channel refresh successfully")
		break
	}
}

func (c *AmqpDrive) CloseChannel(ID string) error {
	c.channelDone[ID] <- 1
	return c.channel[ID].Close()
}

func (c *AmqpDrive) SetConsume(option types.PipeOption) (err error) {
	msgs, err := c.channel[option.Identity].Consume(
		option.Queue,
		option.Identity,
		false,
		false,
		false,
		false,
		nil,
	)
	go func() {
		for d := range msgs {
			println(string(d.Body))
			err = actions.Push(c.client, option.Index, d.Body)
			if err != nil {
				println(err.Error())
				time.Sleep(time.Second * 15)
				d.Nack(false, true)
			} else {
				println("ok")
				d.Ack(false)
			}
		}
	}()
	return
}