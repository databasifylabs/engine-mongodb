package mongodb

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/databasifylabs/engine-sdk/command"
	"github.com/databasifylabs/engine-sdk/dtos"
	"github.com/databasifylabs/engine-sdk/enums"
	"github.com/databasifylabs/engine-sdk/requests"
)

func secret(value string) *string { return &value }

func replicaRequest() requests.NodeConfigure {
	seed := "10.0.0.1"

	return requests.NodeConfigure{
		Clustered: true, Ip: "10.0.0.2", DatabaseId: "db1", Primary: &seed,
		Members: []dtos.Member{
			{Ip: "10.0.0.1", Position: 1, Voting: true, Cpus: 2, MemoryMb: 4096, DiskGb: 80},
			{Ip: "10.0.0.2", Position: 2, Voting: true, Cpus: 2, MemoryMb: 4096, DiskGb: 80},
		},
		Settings: map[string]string{"cacheSizeGB": "1.5", "verbosity": "1"},
		Secrets:  dtos.Secrets{Admin: secret("root-pw"), Replication: secret("repl"), Agent: secret("agent")},
	}
}

func TestMongodConfForAReplicaSetMember(t *testing.T) {
	conf := mongodConfOf("/srv/mongodb", replicaRequest().Settings)

	for _, fragment := range []string{`"replSetName": "databasify"`, `"keyFile": "/srv/mongodb/conf/keyfile"`, `"authorization": "enabled"`, `"mode": "requireTLS"`, `"port": 25060`, `"cacheSizeGB": 1.5`, `"verbosity": 1`, `"dbPath": "/srv/mongodb/data"`} {
		if !strings.Contains(conf, fragment) {
			t.Errorf("mongod.conf lacks %s:\n%s", fragment, conf)
		}
	}
}

func TestKeyfileIsTheSameOnEveryNode(t *testing.T) {
	first, second := keyfileOf("repl"), keyfileOf("repl")

	if first != second || first == keyfileOf("other") || len(first) < 6 || len(first) > 1024 || strings.Contains(first, "repl") {
		t.Fatalf("keyfile = %q", first)
	}
}

func TestMongodbRefusesMoreThanFiftyMembers(t *testing.T) {
	request := replicaRequest()
	for index := range 49 {
		request.Members = append(request.Members, dtos.Member{Ip: "10.0.1." + strconv.Itoa(index), Cpus: 2, MemoryMb: 4096, DiskGb: 80})
	}

	if checkMembers(request) == nil {
		t.Fatal("a 51-member replica set was accepted")
	}
}

func TestGrantSetsExactRoles(t *testing.T) {
	sent := []bson.D{}
	engine := Engine{admin: fakeAdmin{replies: map[string]bson.M{"listDatabases": {"databases": bson.A{bson.M{"name": "shop"}, bson.M{"name": "crm"}}}, "usersInfo": {"users": bson.A{bson.M{"user": "app"}}}}, sent: &sent}}

	err := engine.Accounts().Grant(context.Background(), requests.UserGrant{Name: "app", Grants: []dtos.Grant{{Database: "shop", Access: "write"}, {Database: "crm", Access: "read"}}, Replication: true})
	if err != nil {
		t.Fatal(err)
	}

	update := sent[len(sent)-1]
	roles, _ := bson.Marshal(update)
	text := bson.Raw(roles).String()
	for _, want := range []string{`"updateUser": "app"`, `"role": "readWrite","db": "shop"`, `"role": "read","db": "crm"`, `"role": "clusterMonitor","db": "admin"`} {
		if !strings.Contains(text, want) {
			t.Errorf("update lacks %s: %s", want, text)
		}
	}
}

func TestCreateDatabaseKeepsAPlaceholderCollection(t *testing.T) {
	sent := []bson.D{}
	engine := Engine{admin: fakeAdmin{replies: map[string]bson.M{"listDatabases": {"databases": bson.A{}}}, sent: &sent}}

	created, err := engine.Accounts().CreateDatabase(context.Background(), requests.DatabaseCreate{Name: "shop"})

	if err != nil || !created.Created || sent[len(sent)-1][0].Key != "create" || sent[len(sent)-1][0].Value != placeholderCollection {
		t.Fatalf("created = %+v, err = %v, sent = %v", created, err, sent)
	}
}

func TestOnlyMongodbLimitsAreEnforced(t *testing.T) {
	if err := catalog().Check(map[string]string{"maxIncomingConnections": "1000000", "cacheSizeGB": "64", "defaultWriteConcern": "1"}); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"maxIncomingConnections": "1000001", "cacheSizeGB": "0.1", "verbosity": "6", "defaultReadConcern": "linearizable"} {
		if catalog().Check(map[string]string{key: value}) == nil {
			t.Errorf("%s=%s was accepted", key, value)
		}
	}
}

func TestMongodbTuningFollowsTheNodeSize(t *testing.T) {
	tuned := tunedFor(dtos.Member{Cpus: 2, MemoryMb: 8192})
	small := tunedFor(dtos.Member{Cpus: 1, MemoryMb: 1024})

	if tuned["cacheSizeGB"] != "3.50" || tuned["maxIncomingConnections"] != "4096" || small["cacheSizeGB"] != "0.25" || small["maxIncomingConnections"] != "512" {
		t.Fatalf("tuned = %v, small = %v", tuned, small)
	}
}

func TestDropDatabaseRevokesItsRoles(t *testing.T) {
	sent := []bson.D{}
	engine := Engine{admin: fakeAdmin{replies: map[string]bson.M{
		"listDatabases": {"databases": bson.A{bson.M{"name": "shop"}}},
		"usersInfo": {"users": bson.A{
			bson.M{"user": "app", "roles": bson.A{bson.M{"role": "readWrite", "db": "shop"}, bson.M{"role": "read", "db": "crm"}}},
			bson.M{"user": "other", "roles": bson.A{bson.M{"role": "read", "db": "crm"}}},
		}},
	}, sent: &sent}}

	if _, err := engine.Accounts().DropDatabase(context.Background(), requests.DatabaseDrop{Name: "shop"}); err != nil {
		t.Fatal(err)
	}

	revoke, _ := bson.Marshal(sent[len(sent)-2])
	text := bson.Raw(revoke).String()
	if !strings.Contains(text, `"revokeRolesFromUser": "app"`) || !strings.Contains(text, `"db": "shop"`) || strings.Contains(text, "crm") || sent[len(sent)-1][0].Key != "dropDatabase" {
		t.Fatalf("sent = %v", sent)
	}
}

func TestScramSha256RefusesControlCharacters(t *testing.T) {
	engine := Engine{admin: fakeAdmin{}}

	_, err := engine.Accounts().CreateUser(context.Background(), requests.UserCreate{Name: "app", Password: "line\nbreak"})
	var failure command.Failure
	if !errors.As(err, &failure) || failure.Code != enums.ErrorCodeInvalidRequest {
		t.Fatalf("err = %v", err)
	}
	sha1 := "SCRAM-SHA-1"
	if _, err := engine.Accounts().CreateUser(context.Background(), requests.UserCreate{Name: "app", Password: "line\nbreak", Encryption: &sha1}); err != nil {
		t.Fatalf("SCRAM-SHA-1 refused it: %v", err)
	}
}
