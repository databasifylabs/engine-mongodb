package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	family "github.com/databasifylabs/engine-mysql-family"
	"github.com/databasifylabs/engine-sdk/command"
	"github.com/databasifylabs/engine-sdk/contract"
	"github.com/databasifylabs/engine-sdk/enums"
	"github.com/databasifylabs/engine-sdk/requests"
	"github.com/databasifylabs/engine-sdk/responses"
	"github.com/databasifylabs/engine-sdk/settings"
)

const (
	nodeState      = "state/node.json"
	tunedState     = "state/tuned.json"
	readyTimeout   = 6 * time.Hour
	primaryTimeout = 2 * time.Minute
	maxReplicaSet  = 50
	pbmRole        = "pbmAnyAction"
	pbmUser        = "databasify_pbm"
)

func checkMembers(request requests.NodeConfigure) error {
	if len(request.Members) > maxReplicaSet {
		return command.Invalid(fmt.Sprintf("a MongoDB replica set has at most %d members; got %d", maxReplicaSet, len(request.Members)))
	}

	return nil
}

func (e Engine) saved() (requests.NodeConfigure, error) {
	var request requests.NodeConfigure
	data := e.root.Read(nodeState)
	if data == "" {
		return request, command.Failure{Code: enums.ErrorCodeBusy, Message: "run node configure first"}
	}

	return request, json.Unmarshal([]byte(data), &request)
}

func (e Engine) effective(request requests.NodeConfigure) (map[string]string, error) {
	tuned, err := catalog().Tuned(e.root.Path(tunedState), request.Self())

	return settings.Effective(tuned, request.Settings), err
}

func (e nodes) Configure(_ context.Context, request requests.NodeConfigure) (responses.NodeConfigure, error) {
	if request.Secrets.Admin == nil || request.Secrets.Replication == nil {
		return responses.NodeConfigure{}, command.Invalid("MongoDB needs the admin and replication secrets")
	}
	if err := checkMembers(request); err != nil {
		return responses.NodeConfigure{}, err
	}
	if err := catalog().Check(request.Settings); err != nil {
		return responses.NodeConfigure{}, err
	}
	for _, dir := range []string{"conf", "data", "state"} {
		if err := e.root.Dir(dir); err != nil {
			return responses.NodeConfigure{}, err
		}
	}
	effective, err := e.effective(request)
	if err != nil {
		return responses.NodeConfigure{}, err
	}
	state, err := json.Marshal(request)
	if err != nil {
		return responses.NodeConfigure{}, err
	}
	encoded, err := manifestOf(string(e.root), request)
	if err != nil {
		return responses.NodeConfigure{}, err
	}
	changed := false
	for _, file := range []struct {
		path        string
		content     string
		mode        os.FileMode
		engineOwned bool
	}{
		{mongodConf, mongodConfOf(string(e.root), effective), 0o600, true},
		{keyfilePath, keyfileOf(*request.Secrets.Replication), 0o400, true},
		{adminPassword, *request.Secrets.Admin, 0o600, false},
		{manifestFile, string(encoded), 0o600, false},
		{nodeState, string(state), 0o600, false},
	} {
		written, err := e.root.Write(file.path, file.content, file.mode, file.engineOwned)
		if err != nil {
			return responses.NodeConfigure{}, err
		}
		changed = changed || written
	}

	if err := e.supervisor.Do("daemon-reload"); err != nil {
		return responses.NodeConfigure{}, err
	}

	return responses.NodeConfigure{Changed: changed}, e.startAgentIfServing(request)
}

func (e Engine) startAgentIfServing(request requests.NodeConfigure) error {
	if request.Archive == nil || request.Secrets.Agent == nil || !e.supervisor.Active(serverUnit) {
		return nil
	}

	return e.supervisor.Do("start", agentUnit)
}

func (e nodes) Start(context.Context) error {
	if err := e.supervisor.Do("start", serverUnit); err != nil {
		return err
	}
	if request, err := e.saved(); err == nil && request.Archive != nil {
		return e.supervisor.Do("start", agentUnit)
	}

	return nil
}

func (e nodes) Ready(ctx context.Context, _ requests.NodeReady, report contract.Report) error {
	report("waiting for MongoDB")
	baseline := e.restarts()
	err := family.Until(ctx, readyTimeout, func() (bool, error) {
		if e.restarts()-baseline > 3 {
			return false, errors.New("MongoDB keeps stopping while it starts, see its log")
		}
		state, err := e.anonymousHello(ctx)
		if err != nil {
			return false, nil
		}

		return state.SetName == "" || state.IsWritablePrimary || state.Secondary, nil
	})

	return err
}

func (e Engine) restarts() int {
	restarts, _ := strconv.Atoi(e.supervisor.Property(serverUnit, "NRestarts"))

	return restarts
}

func (e Engine) anonymousHello(ctx context.Context) (hello, error) {
	var reply hello

	return reply, e.anonymous.Run(ctx, "admin", bson.D{{Key: "hello", Value: 1}}, &reply)
}

func (e nodes) Wake(context.Context, requests.NodeWake) error {
	_ = e.supervisor.Kill("KILL", serverUnit)

	return e.supervisor.Do("stop", serverUnit)
}

func (e nodes) Bootstrap(ctx context.Context, request requests.NodeBootstrap) error {
	if request.Secrets.Admin == nil || request.Secrets.Agent == nil {
		return command.Invalid("MongoDB needs the admin and agent secrets")
	}
	saved, err := e.saved()
	if err != nil || !saved.Seed() {
		return err
	}
	if err := e.initiate(ctx, saved); err != nil {
		return err
	}
	if err := e.ensureRoot(ctx, *request.Secrets.Admin); err != nil {
		return err
	}
	if err := e.ensureAgent(ctx, *request.Secrets.Agent); err != nil {
		return err
	}
	return contract.CreateDefaultAccount(ctx, e.Accounts(), request.Account)
}

func (e Engine) initiate(ctx context.Context, request requests.NodeConfigure) error {
	state, err := e.anonymousHello(ctx)
	if err != nil {
		return err
	}
	if state.SetName == "" {
		config := bson.D{{Key: "_id", Value: replicaSet}, {Key: "members", Value: bson.A{bson.D{{Key: "_id", Value: 0}, {Key: "host", Value: hostOf(request.Ip)}}}}}
		if err := e.anonymous.Run(ctx, "admin", bson.D{{Key: "replSetInitiate", Value: config}}, nil); err != nil {
			return err
		}
	}

	return family.Until(ctx, primaryTimeout, func() (bool, error) {
		state, err := e.anonymousHello(ctx)

		return err == nil && state.IsWritablePrimary, nil
	})
}

func hostOf(ip string) string {
	return ip + ":" + strconv.Itoa(clientPort)
}

func (e Engine) ensureRoot(ctx context.Context, password string) error {
	create := bson.D{{Key: "createUser", Value: "root"}, {Key: "pwd", Value: password}, {Key: "roles", Value: bson.A{bson.D{{Key: "role", Value: "root"}, {Key: "db", Value: "admin"}}}}}
	err := e.anonymous.Run(ctx, "admin", create, nil)
	if err == nil {
		return nil
	}

	return e.admin.Run(ctx, "admin", bson.D{{Key: "updateUser", Value: "root"}, {Key: "pwd", Value: password}}, nil)
}

func (e Engine) ensureAgent(ctx context.Context, password string) error {
	role := bson.D{
		{Key: "createRole", Value: pbmRole},
		{Key: "privileges", Value: bson.A{bson.D{{Key: "resource", Value: bson.D{{Key: "anyResource", Value: true}}}, {Key: "actions", Value: bson.A{"anyAction"}}}}},
		{Key: "roles", Value: bson.A{}},
	}
	if err := e.admin.Run(ctx, "admin", role, nil); err != nil && !alreadyExists(err) {
		return err
	}
	roles := bson.A{
		bson.D{{Key: "role", Value: "backup"}, {Key: "db", Value: "admin"}},
		bson.D{{Key: "role", Value: "restore"}, {Key: "db", Value: "admin"}},
		bson.D{{Key: "role", Value: "clusterMonitor"}, {Key: "db", Value: "admin"}},
		bson.D{{Key: "role", Value: "readWrite"}, {Key: "db", Value: "admin"}},
		bson.D{{Key: "role", Value: pbmRole}, {Key: "db", Value: "admin"}},
	}
	err := e.admin.Run(ctx, "admin", bson.D{{Key: "createUser", Value: pbmUser}, {Key: "pwd", Value: password}, {Key: "roles", Value: roles}}, nil)
	if !alreadyExists(err) {
		return err
	}

	return e.admin.Run(ctx, "admin", bson.D{{Key: "updateUser", Value: pbmUser}, {Key: "pwd", Value: password}, {Key: "roles", Value: roles}}, nil)
}

func alreadyExists(err error) bool {
	var failure command.Failure

	return errors.As(err, &failure) && failure.Code == enums.ErrorCodeAlreadyExists
}

func (e nodes) SyncConfig(context.Context) (responses.ConfigSync, error) {
	return responses.ConfigSync{}, nil
}

func (e nodes) ApplyConfig(ctx context.Context) error {
	saved, err := e.saved()
	if err != nil {
		return err
	}
	effective, err := e.effective(saved)
	if err != nil {
		return err
	}
	valueOf := func(key string) string {
		if value, set := effective[key]; set {
			return value
		}
		for _, field := range catalog().Fields {
			if field.Key == key {
				return field.Default
			}
		}

		return ""
	}
	verbosity, _ := strconv.Atoi(valueOf(keyVerbosity))
	lifetime, _ := strconv.Atoi(valueOf(keyTransactions))
	slow, _ := strconv.Atoi(valueOf(keySlowOperation))
	cache, _ := strconv.ParseFloat(valueOf(keyCacheSize), 64)
	commands := []bson.D{
		{{Key: "setParameter", Value: 1}, {Key: "logLevel", Value: verbosity}},
		{{Key: "setParameter", Value: 1}, {Key: keyTransactions, Value: lifetime}},
		{{Key: "setParameter", Value: 1}, {Key: "wiredTigerEngineRuntimeConfig", Value: fmt.Sprintf("cache_size=%dM", int(cache*1024))}},
		{{Key: "profile", Value: -1}, {Key: "slowms", Value: slow}},
	}
	for _, cmd := range commands {
		if err := e.admin.Run(ctx, "admin", cmd, nil); err != nil {
			return err
		}
	}
	state, err := e.hello(ctx)
	if err != nil || !state.IsWritablePrimary {
		return err
	}

	return e.admin.Run(ctx, "admin", bson.D{
		{Key: "setDefaultRWConcern", Value: 1},
		{Key: "defaultReadConcern", Value: bson.D{{Key: "level", Value: valueOf(keyReadConcern)}}},
		{Key: "defaultWriteConcern", Value: bson.D{{Key: "w", Value: writeConcernOf(valueOf(keyWriteConcern))}}},
	}, nil)
}

func writeConcernOf(value string) any {
	if number, err := strconv.Atoi(value); err == nil {
		return number
	}

	return value
}

func (e nodes) InstallTLS(ctx context.Context, request requests.TlsInstall) error {
	return e.root.InstallTLS(ctx, request, true, e.ReloadTLS)
}

func (e nodes) Restart(context.Context) error {
	return e.supervisor.Do("restart", serverUnit)
}

func (e nodes) ReloadTLS(ctx context.Context) error {
	if !e.supervisor.Active(serverUnit) {
		return nil
	}

	return e.admin.Run(ctx, "admin", bson.D{{Key: "rotateCertificates", Value: 1}}, nil)
}

func (e nodes) Reset(context.Context, requests.NodeReset) error {
	return contract.ErrUnsupported
}
