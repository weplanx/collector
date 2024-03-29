// Code generated by Wire. DO NOT EDIT.

//go:generate go run -mod=mod github.com/google/wire/cmd/wire
//go:build !wireinject
// +build !wireinject

package bootstrap

import (
	"github.com/weplanx/collector/app"
	"github.com/weplanx/collector/common"
)

// Injectors from wire.go:

func NewApp() (*app.App, error) {
	values, err := LoadStaticValues()
	if err != nil {
		return nil, err
	}
	logger, err := UseZap()
	if err != nil {
		return nil, err
	}
	client, err := UseMongoDB(values)
	if err != nil {
		return nil, err
	}
	database := UseDatabase(values, client)
	conn, err := UseNats(values)
	if err != nil {
		return nil, err
	}
	jetStreamContext, err := UseJetStream(conn)
	if err != nil {
		return nil, err
	}
	keyValue, err := UseKeyValue(jetStreamContext)
	if err != nil {
		return nil, err
	}
	inject := &common.Inject{
		Values:    values,
		Log:       logger,
		Db:        database,
		JetStream: jetStreamContext,
		KeyValue:  keyValue,
	}
	appApp := app.Initialize(inject)
	return appApp, nil
}
