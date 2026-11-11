package mongodb

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/databasifylabs/engine-sdk/command"
	"github.com/databasifylabs/engine-sdk/enums"
	"github.com/databasifylabs/engine-sdk/responses"
)

type hello struct {
	IsWritablePrimary bool   `bson:"isWritablePrimary"`
	Secondary         bool   `bson:"secondary"`
	SetName           string `bson:"setName"`
}

type buildInfo struct {
	Version string `bson:"version"`
}

type serverStatus struct {
	Connections struct {
		Current int64 `bson:"current"`
	} `bson:"connections"`
	Opcounters map[string]int64 `bson:"opcounters"`
}

type databaseList struct {
	TotalSize int64 `bson:"totalSize"`
}

func (e Engine) hello(ctx context.Context) (hello, error) {
	var reply hello

	return reply, e.admin.Run(ctx, "admin", bson.D{{Key: "hello", Value: 1}}, &reply)
}

func (e nodes) Status(ctx context.Context) (responses.NodeStatus, error) {
	state, err := e.anonymousHello(ctx)
	if isUnavailable(err) {
		return responses.NodeStatus{Role: enums.NodeRoleOffline}, nil
	}
	if err != nil {
		return responses.NodeStatus{}, err
	}
	var info buildInfo
	if err := e.member.Run(ctx, "admin", bson.D{{Key: "buildInfo", Value: 1}}, &info); err != nil {
		return responses.NodeStatus{}, err
	}

	return responses.NodeStatus{Role: roleOf(state), Version: &info.Version}, nil
}

func roleOf(state hello) enums.NodeRole {
	switch {
	case state.IsWritablePrimary:
		return enums.NodeRolePrimary
	case state.Secondary:
		return enums.NodeRoleReplica
	}

	return enums.NodeRoleUnknown
}

func (e nodes) Metrics(ctx context.Context) (responses.NodeMetrics, error) {
	var status serverStatus
	if err := e.admin.Run(ctx, "admin", bson.D{{Key: "serverStatus", Value: 1}}, &status); err != nil {
		return responses.NodeMetrics{}, err
	}
	var databases databaseList
	if err := e.admin.Run(ctx, "admin", bson.D{{Key: "listDatabases", Value: 1}, {Key: "nameOnly", Value: false}}, &databases); err != nil {
		return responses.NodeMetrics{}, err
	}
	var queries int64
	for _, count := range status.Opcounters {
		queries += count
	}

	return responses.NodeMetrics{Connections: status.Connections.Current, Queries: queries, DataMb: databases.TotalSize / (1 << 20)}, nil
}

func isUnavailable(err error) bool {
	var failure command.Failure

	return errors.As(err, &failure) && failure.Code == enums.ErrorCodeUnavailable
}
