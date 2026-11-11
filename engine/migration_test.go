package mongodb

import (
	"context"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/databasifylabs/engine-sdk/dtos"
	"github.com/databasifylabs/engine-sdk/requests"
)

func sourceEngine(version string, setName string) Engine {
	source := fakeAdmin{replies: map[string]bson.M{
		"buildInfo":     {"version": version},
		"hello":         {"setName": setName},
		"listDatabases": {"databases": bson.A{bson.M{"name": "shop", "sizeOnDisk": 3 << 20}, bson.M{"name": "admin", "sizeOnDisk": 1 << 20}}},
	}}
	local := fakeAdmin{replies: map[string]bson.M{"listCollections": {"cursor": bson.M{"firstBatch": bson.A{bson.M{"name": "_databasify"}}}}}}

	return Engine{admin: local, line: "8.0", remote: func(string) Admin { return source }}
}

func TestContinuousNeedsTheSameMajor(t *testing.T) {
	request := requests.MigrationCheck{Source: dtos.MigrationSource{Host: "old.example.com", Port: 27017, Username: "app"}}

	same, err := sourceEngine("8.0.12", "rs0").Migration().Check(context.Background(), request)
	if err != nil || !same.CanReplicate || len(same.Warnings) != 0 || !slices.Equal(same.Databases, []string{"shop"}) || *same.SizeMb != 3 || len(same.Occupied) != 0 {
		t.Fatalf("same major: %+v, %v", same, err)
	}

	older, _ := sourceEngine("7.0.20", "rs0").Migration().Check(context.Background(), request)
	if older.CanReplicate || len(older.Warnings) != 1 {
		t.Fatalf("older major: %+v", older)
	}

	standalone, _ := sourceEngine("8.0.12", "").Migration().Check(context.Background(), request)
	if standalone.CanReplicate || len(standalone.Warnings) != 1 {
		t.Fatalf("standalone: %+v", standalone)
	}
}

func TestPcsmUrisStayInTheEnvironment(t *testing.T) {
	pcsm := pcsmCommand(context.Background(), "mongodb://app:source-secret@old:27017/", "mongodb://root:target-secret@127.0.0.1:25060/")

	if strings.Contains(strings.Join(pcsm.Args, " "), "secret") {
		t.Fatalf("argv carries a password: %v", pcsm.Args)
	}
	if !slices.Contains(pcsm.Env, "PCSM_SOURCE_URI=mongodb://app:source-secret@old:27017/") || !slices.Contains(pcsm.Env, "PCSM_TARGET_URI=mongodb://root:target-secret@127.0.0.1:25060/") {
		t.Fatal("the URIs are missing from the environment")
	}
}

func TestOneShotPipesDumpIntoRestore(t *testing.T) {
	dump, restore := dumpArguments("/m/source.yaml", "shop"), restoreArguments("/m/target.yaml", "shop")

	if !slices.Equal(dump, []string{"--config=/m/source.yaml", "--archive", "--gzip", "--db=shop"}) {
		t.Errorf("dump %v", dump)
	}
	for _, flag := range []string{"--config=/m/target.yaml", "--archive", "--gzip", "--nsInclude=shop.*", "--drop"} {
		if !slices.Contains(restore, flag) {
			t.Errorf("restore %v lacks %s", restore, flag)
		}
	}
	if toolConfigOf(`mongodb://a:p"w@h/`) != "uri: \"mongodb://a:p\\\"w@h/\"\n" {
		t.Fatalf("config = %q", toolConfigOf(`mongodb://a:p"w@h/`))
	}
}
