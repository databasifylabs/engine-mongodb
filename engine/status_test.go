package mongodb

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/databasifylabs/engine-sdk/command"
	"github.com/databasifylabs/engine-sdk/enums"
)

type fakeAdmin struct {
	replies map[string]bson.M
	err     error
	sent    *[]bson.D
}

func (f fakeAdmin) Run(_ context.Context, _ string, cmd bson.D, result any) error {
	if f.sent != nil {
		*f.sent = append(*f.sent, cmd)
	}
	if f.err != nil {
		return f.err
	}
	encoded, err := bson.Marshal(f.replies[cmd[0].Key])
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}

	return bson.Unmarshal(encoded, result)
}

func TestStatusFromHello(t *testing.T) {
	cases := map[enums.NodeRole]bson.M{
		enums.NodeRolePrimary: {"isWritablePrimary": true},
		enums.NodeRoleReplica: {"secondary": true},
		enums.NodeRoleUnknown: {"isWritablePrimary": false},
	}
	for want, hello := range cases {
		anonymous := fakeAdmin{replies: map[string]bson.M{"hello": hello}}
		member := fakeAdmin{replies: map[string]bson.M{"buildInfo": {"version": "8.0.34"}}}
		engine := Engine{admin: fakeAdmin{err: command.Unavailable("no users yet")}, anonymous: anonymous, member: member}

		status, err := engine.Nodes().Status(context.Background())

		if err != nil || status.Role != want || status.Version == nil || *status.Version != "8.0.34" {
			t.Errorf("%s: status = %+v, err = %v", want, status, err)
		}
	}
}

func TestOfflineWhenRefused(t *testing.T) {
	engine := Engine{admin: fakeAdmin{err: command.Unavailable("refused")}, anonymous: fakeAdmin{err: command.Unavailable("refused")}}

	status, err := engine.Nodes().Status(context.Background())

	if err != nil || status.Role != enums.NodeRoleOffline {
		t.Fatalf("status = %+v, err = %v", status, err)
	}
}

func TestMetricsSumOpcounters(t *testing.T) {
	engine := Engine{admin: fakeAdmin{replies: map[string]bson.M{
		"serverStatus":  {"connections": bson.M{"current": 7}, "opcounters": bson.M{"insert": 10, "query": 20, "update": 3, "delete": 1, "getmore": 4, "command": 50}},
		"listDatabases": {"totalSize": int64(3 * 1048576)},
	}}}

	metrics, err := engine.Nodes().Metrics(context.Background())

	if err != nil || metrics.Connections != 7 || metrics.Queries != 88 || metrics.DataMb != 3 {
		t.Fatalf("metrics = %+v, err = %v", metrics, err)
	}
}
