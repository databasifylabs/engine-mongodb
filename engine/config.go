package mongodb

import (
	"context"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/databasifylabs/engine-sdk/contract"
	"github.com/databasifylabs/engine-sdk/dtos"
	"github.com/databasifylabs/engine-sdk/responses"
)

type config struct{ Engine }

func (e Engine) Config() contract.Config { return config{e} }

type liveParameters struct {
	LogLevel     int64 `bson:"logLevel"`
	Transactions int64 `bson:"transactionLifetimeLimitSeconds"`
}

type liveCache struct {
	WiredTiger struct {
		Cache struct {
			Maximum float64 `bson:"maximum bytes configured"`
		} `bson:"cache"`
	} `bson:"wiredTiger"`
}

type liveProfile struct {
	Slowms int64 `bson:"slowms"`
}

type liveOptions struct {
	Parsed struct {
		Net struct {
			MaxIncomingConnections int64 `bson:"maxIncomingConnections"`
		} `bson:"net"`
	} `bson:"parsed"`
}

type liveConcern struct {
	DefaultReadConcern struct {
		Level string `bson:"level"`
	} `bson:"defaultReadConcern"`
	DefaultWriteConcern struct {
		W any `bson:"w"`
	} `bson:"defaultWriteConcern"`
}

func (e config) Fields(context.Context) (responses.ConfigFields, error) {
	request, err := e.saved()
	if err != nil {
		return responses.ConfigFields{Fields: catalog().Fields}, nil
	}
	tuned, err := catalog().Tuned(e.root.Path(tunedState), request.Self())

	return responses.ConfigFields{Fields: catalog().FieldsWith(tuned)}, err
}

func (e config) live(ctx context.Context) (map[string]string, error) {
	var parameters liveParameters
	if err := e.admin.Run(ctx, "admin", bson.D{{Key: "getParameter", Value: 1}, {Key: "logLevel", Value: 1}, {Key: keyTransactions, Value: 1}}, &parameters); err != nil {
		return nil, err
	}
	var cache liveCache
	if err := e.admin.Run(ctx, "admin", bson.D{{Key: "serverStatus", Value: 1}}, &cache); err != nil {
		return nil, err
	}
	var profile liveProfile
	if err := e.admin.Run(ctx, "admin", bson.D{{Key: "profile", Value: -1}}, &profile); err != nil {
		return nil, err
	}
	var options liveOptions
	if err := e.admin.Run(ctx, "admin", bson.D{{Key: "getCmdLineOpts", Value: 1}}, &options); err != nil {
		return nil, err
	}
	var concern liveConcern
	if err := e.admin.Run(ctx, "admin", bson.D{{Key: "getDefaultRWConcern", Value: 1}}, &concern); err != nil {
		return nil, err
	}

	return map[string]string{
		keyVerbosity:     strconv.FormatInt(parameters.LogLevel, 10),
		keyTransactions:  strconv.FormatInt(parameters.Transactions, 10),
		keyCacheSize:     strconv.FormatFloat(cache.WiredTiger.Cache.Maximum/(1<<30), 'f', 2, 64),
		keySlowOperation: strconv.FormatInt(profile.Slowms, 10),
		keyConnections:   strconv.FormatInt(options.Parsed.Net.MaxIncomingConnections, 10),
		keyReadConcern:   concern.DefaultReadConcern.Level,
		keyWriteConcern:  strings.Trim(bsonText(concern.DefaultWriteConcern.W), `"`),
	}, nil
}

func bsonText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case int32:
		return strconv.Itoa(int(typed))
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	}

	return ""
}

func (e config) Show(ctx context.Context) (responses.ConfigShow, error) {
	request, err := e.saved()
	if err != nil {
		return responses.ConfigShow{}, err
	}
	tuned, err := catalog().Tuned(e.root.Path(tunedState), request.Self())
	if err != nil {
		return responses.ConfigShow{}, err
	}
	effective, err := e.effective(request)
	if err != nil {
		return responses.ConfigShow{}, err
	}
	live, err := e.live(ctx)
	if err != nil {
		return responses.ConfigShow{}, err
	}
	values := []dtos.ConfigValue{}
	for _, field := range catalog().Fields {
		wanted, set := effective[field.Key]
		values = append(values, dtos.ConfigValue{
			Key: field.Key, Value: live[field.Key], Source: catalog().SourceOf(field.Key, tuned, request.Settings),
			PendingRestart: field.Restart && set && live[field.Key] != wanted,
		})
	}

	return responses.ConfigShow{Values: values}, nil
}

func (e config) Tune(ctx context.Context) (responses.ConfigTune, error) {
	request, err := e.saved()
	if err != nil {
		return responses.ConfigTune{}, err
	}
	previous, err := catalog().Tuned(e.root.Path(tunedState), request.Self())
	if err != nil {
		return responses.ConfigTune{}, err
	}
	tuned, err := catalog().Retune(e.root.Path(tunedState), request.Self())
	if err != nil {
		return responses.ConfigTune{}, err
	}
	if _, err := e.Nodes().Configure(ctx, request); err != nil {
		return responses.ConfigTune{}, err
	}
	if err := e.Nodes().ApplyConfig(ctx); err != nil {
		return responses.ConfigTune{}, err
	}

	return responses.ConfigTune{Tuned: tuned, Restart: catalog().RestartChanged(previous, tuned, request.Settings)}, nil
}
