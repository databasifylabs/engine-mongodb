package mongodb

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/databasifylabs/engine-sdk/ctl"
	"github.com/databasifylabs/engine-sdk/dtos"
	"github.com/databasifylabs/engine-sdk/nodefs"
	"github.com/databasifylabs/engine-sdk/requests"
)

func TestPbmConfigFromStorage(t *testing.T) {
	config := pbmConfigOf(dtos.Storage{Endpoint: "https://s3.local:9000", Region: "us-east-1", Bucket: "backups", Path: "mg1", AccessKey: "key", SecretKey: "secret", PathStyle: true})

	for _, fragment := range []string{`"bucket": "backups"`, `"prefix": "mg1"`, `"endpointUrl": "https://s3.local:9000"`, `"forcePathStyle": true`, `"insecureSkipTLSVerify": true`, `"access-key-id": "key"`, `"enabled": true`, `"oplogSpanMin": 10`, `"compression": "zstd"`} {
		if !strings.Contains(config, fragment) {
			t.Errorf("pbm config lacks %s:\n%s", fragment, config)
		}
	}
}

func fakePbm(calls *[]string) PbmRunner {
	return func(_ context.Context, _ string, args ...string) (string, error) {
		switch args[0] {
		case "logs":
			return `[{"ts":` + strconv.FormatInt(time.Now().Unix(), 10) + `,"s":3,"msg":"succeed"}]`, nil
		case "status":
			return `{"running":{}}`, nil
		}
		*calls = append(*calls, strings.Join(args, " "))

		return "", nil
	}
}

func archivedEngine(t *testing.T, pbm PbmRunner) Engine {
	t.Helper()
	engine := Engine{root: nodefs.Root(t.TempDir()), pbm: pbm}
	for _, dir := range []string{"conf", "state"} {
		if err := engine.root.Dir(dir); err != nil {
			t.Fatal(err)
		}
	}
	request := replicaRequest()
	request.Archive = &dtos.ArchiveSettings{Storage: dtos.Storage{Bucket: "own"}, RetentionDays: 7}
	saved, _ := json.Marshal(request)
	if _, err := engine.root.Write(nodeState, string(saved), 0o600, false); err != nil {
		t.Fatal(err)
	}

	return engine
}

func TestRestoreTurnsPitrOffThenOn(t *testing.T) {
	calls, sent := []string{}, []bson.D{}
	engine := archivedEngine(t, fakePbm(&calls))
	engine.member = fakeAdmin{sent: &sent}
	target := time.Date(2026, 11, 9, 10, 30, 0, 0, time.UTC)

	if err := engine.Archive().Restore(context.Background(), requests.RestoreStart{Key: "r1", Target: target}, func(string) {}); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"config --file " + engine.root.Path(pbmConfig),
		"config --force-resync",
		"config --set pitr.enabled=false",
		"restore --time=2026-11-09T10:30:00 --yes --wait",
		"config --file " + engine.root.Path(pbmConfig),
		"config --force-resync",
		"config --set pitr.enabled=true",
		"backup --compression=zstd --wait",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("pbm calls:\n%s", strings.Join(calls, "\n"))
	}
	if !strings.Contains(engine.root.Read(pbmConfig), `"bucket": "own"`) {
		t.Fatalf("restore left the source storage configured:\n%s", engine.root.Read(pbmConfig))
	}
	reclaimed, _ := bson.Marshal(bson.D{{Key: "sent", Value: sent}})
	for _, want := range []string{`"root","pwd": "root-pw"`, `"createUser": "databasify_pbm","pwd": "agent"`} {
		if !strings.Contains(bson.Raw(reclaimed).String(), want) {
			t.Errorf("restore did not put back %s: %s", want, bson.Raw(reclaimed).String())
		}
	}
}

func TestResyncFailureStopsTheWait(t *testing.T) {
	engine := archivedEngine(t, func(context.Context, string, ...string) (string, error) {
		return `[{"ts":` + strconv.FormatInt(time.Now().Unix(), 10) + `,"s":1,"msg":"access denied"}]`, nil
	})

	if _, err := (archive{engine}).resynced(context.Background(), time.Now().Unix()-1); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("err = %v", err)
	}
}

func TestRestoreNeedsArchiveOnTheTarget(t *testing.T) {
	engine := Engine{root: nodefs.Root(t.TempDir()), pbm: func(context.Context, string, ...string) (string, error) {
		t.Fatal("pbm ran without an archive")
		return "", nil
	}}
	if err := engine.root.Dir("state"); err != nil {
		t.Fatal(err)
	}
	saved, _ := json.Marshal(replicaRequest())
	if _, err := engine.root.Write(nodeState, string(saved), 0o600, false); err != nil {
		t.Fatal(err)
	}

	if engine.Archive().Restore(context.Background(), requests.RestoreStart{Key: "r1"}, func(string) {}) == nil {
		t.Fatal("restore ran without an archive")
	}
}

func TestStatusParsesPbmJson(t *testing.T) {
	raw := `{"cluster":[{"rs":"databasify","nodes":[{"host":"10.0.0.1:25060","ok":false,"errors":["storage: unreachable"]}]}],"pitr":{"conf":true,"run":true},"backups":{"snapshot":[{"name":"a","status":"done","restoreTo":1762678800},{"name":"b","status":"done","restoreTo":1762765200},{"name":"c","status":"error","restoreTo":1762851600}],"pitrChunks":{"pitrChunks":[{"range":{"start":1762765200,"end":1762768800}}]}}}`

	status, err := statusOf(raw)

	if err != nil {
		t.Fatal(err)
	}
	if status.Backups != 2 || status.Earliest.Unix() != 1762678800 || status.LastBackup.Unix() != 1762765200 || status.Latest.Unix() != 1762768800 || status.Problem != nil {
		t.Fatalf("status = %+v", status)
	}
}

type servingSupervisor struct {
	serving bool
	calls   *[]string
}

func (s servingSupervisor) Do(verb string, units ...string) error {
	*s.calls = append(*s.calls, verb+" "+strings.Join(units, " "))

	return nil
}

func (s servingSupervisor) Run(ctl.Transient) error                  { return nil }
func (s servingSupervisor) DoNoBlock(verb string, u ...string) error { return s.Do(verb, u...) }
func (s servingSupervisor) Kill(string, ...string) error             { return nil }
func (s servingSupervisor) Active(unit string) bool                  { return s.serving && unit == serverUnit }
func (s servingSupervisor) Property(string, string) string           { return "" }

func TestConfigureStartsTheBackupAgentOnAServingNode(t *testing.T) {
	agent := "a"
	archived := requests.NodeConfigure{Archive: &dtos.ArchiveSettings{}, Secrets: dtos.Secrets{Agent: &agent}}
	for serving, want := range map[bool][]string{true: {"start " + agentUnit}, false: {}} {
		calls := []string{}
		engine := Engine{supervisor: servingSupervisor{serving: serving, calls: &calls}}

		if err := engine.startAgentIfServing(archived); err != nil {
			t.Fatal(err)
		}

		if !slices.Equal(calls, want) {
			t.Errorf("serving %v: calls = %v", serving, calls)
		}
	}
}
