package mongodb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/databasifylabs/engine-sdk/command"
	"github.com/databasifylabs/engine-sdk/enums"
)

const (
	uriPattern                  = "mongodb://%s/?directConnection=true&tls=true&tlsInsecure=true"
	commandTimeout              = 30 * time.Second
	codeNotPrimary              = 10107
	codeNotPrimaryNoSecondaryOk = 13435
	codeDuplicateKey            = 11000
	codeUserExists              = 51003
	codeRoleExists              = 51002
)

type Admin interface {
	Run(ctx context.Context, database string, cmd bson.D, result any) error
}

type localAdmin struct {
	host         string
	passwordFile string
	anonymous    bool
	member       bool
}

func (a localAdmin) Run(ctx context.Context, database string, cmd bson.D, result any) error {
	host := a.host
	if host == "" {
		host = "127.0.0.1:25060"
	}
	clientOptions := options.Client().ApplyURI(fmt.Sprintf(uriPattern, host))
	if password := a.password(); password != "" && !a.anonymous {
		clientOptions.SetAuth(a.credentialFor(password))
	}

	return runWith(ctx, clientOptions, database, cmd, result)
}

type remoteAdmin struct{ uri string }

func (a remoteAdmin) Run(ctx context.Context, database string, cmd bson.D, result any) error {
	return runWith(ctx, options.Client().ApplyURI(a.uri), database, cmd, result)
}

func runWith(ctx context.Context, clientOptions *options.ClientOptions, database string, cmd bson.D, result any) error {
	client, err := mongo.Connect(clientOptions.SetTimeout(commandTimeout).SetServerSelectionTimeout(5 * time.Second))
	if err != nil {
		return failureOf(err)
	}
	defer func() { _ = client.Disconnect(context.WithoutCancel(ctx)) }()
	reply := client.Database(database).RunCommand(ctx, cmd)
	if result == nil {
		return failureOf(reply.Err())
	}

	return failureOf(reply.Decode(result))
}

func (a localAdmin) credentialFor(password string) options.Credential {
	if a.member {
		return options.Credential{Username: "__system", Password: password, AuthSource: "local", AuthMechanism: "SCRAM-SHA-256"}
	}

	return options.Credential{Username: "root", Password: password, AuthSource: "admin"}
}

func (a localAdmin) password() string {
	data, err := os.ReadFile(a.passwordFile)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(data))
}

func failureOf(err error) error {
	if err == nil {
		return nil
	}
	var server mongo.ServerError
	switch {
	case errors.As(err, &server) && (server.HasErrorCode(codeNotPrimary) || server.HasErrorCode(codeNotPrimaryNoSecondaryOk)):
		return command.NotPrimary(err.Error())
	case errors.As(err, &server) && (server.HasErrorCode(codeDuplicateKey) || server.HasErrorCode(codeUserExists) || server.HasErrorCode(codeRoleExists)):
		return command.Failure{Code: enums.ErrorCodeAlreadyExists, Message: err.Error()}
	case errors.Is(err, syscall.ECONNREFUSED) || mongo.IsNetworkError(err) || errors.Is(err, mongo.ErrClientDisconnected) || strings.Contains(err.Error(), "server selection error"):
		return command.Unavailable(err.Error())
	}

	return err
}
