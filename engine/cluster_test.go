package mongodb

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/databasifylabs/engine-sdk/command"
	"github.com/databasifylabs/engine-sdk/dtos"
	"github.com/databasifylabs/engine-sdk/enums"
	"github.com/databasifylabs/engine-sdk/nodefs"
	"github.com/databasifylabs/engine-sdk/requests"
)

func setConfig(members ...bson.M) setConfiguration {
	list := []bson.M{}
	list = append(list, members...)

	return setConfiguration{"_id": replicaSet, "version": int64(3), "members": list}
}

func TestAdmitAddsWithoutAVote(t *testing.T) {
	config := setConfig(bson.M{"_id": int64(0), "host": "10.0.0.1:25060", "votes": int64(1), "priority": float64(1)})

	added, changed := withMember(config, "10.0.0.2:25060")

	member := added.member("10.0.0.2:25060")
	if !changed || member == nil || member["votes"] != 0 || member["priority"] != 0 || member["_id"] != int64(1) || added["version"] != int64(4) {
		t.Fatalf("config = %+v", added)
	}
	if _, again := withMember(added, "10.0.0.2:25060"); again {
		t.Fatal("a member already in the set was added twice")
	}
}

func TestPromoteOnlyWhenSecondary(t *testing.T) {
	config := setConfig(
		bson.M{"_id": int64(0), "host": "10.0.0.1:25060", "votes": int64(1), "priority": float64(1)},
		bson.M{"_id": int64(1), "host": "10.0.0.2:25060", "votes": int64(0), "priority": float64(0)},
	)

	if _, changed := withVote(config, "10.0.0.2:25060", false); changed {
		t.Fatal("a member that is not yet secondary got a vote")
	}
	promoted, changed := withVote(config, "10.0.0.2:25060", true)
	member := promoted.member("10.0.0.2:25060")
	if !changed || member["votes"] != 1 || member["priority"] != 1 {
		t.Fatalf("config = %+v", promoted)
	}
}

func TestForceQuorumRefusesWithAHealthyMajority(t *testing.T) {
	engine := Engine{admin: fakeAdmin{replies: map[string]bson.M{
		"replSetGetStatus": {"members": bson.A{
			bson.M{"name": "10.0.0.1:25060", "health": 1, "stateStr": "SECONDARY"},
			bson.M{"name": "10.0.0.2:25060", "health": 1, "stateStr": "SECONDARY"},
			bson.M{"name": "10.0.0.3:25060", "health": 0, "stateStr": "(not reachable/healthy)"},
		}},
		"replSetGetConfig": {"config": bson.M{"_id": replicaSet, "version": 3, "members": bson.A{
			bson.M{"_id": 0, "host": "10.0.0.1:25060", "votes": 1}, bson.M{"_id": 1, "host": "10.0.0.2:25060", "votes": 1}, bson.M{"_id": 2, "host": "10.0.0.3:25060", "votes": 1},
		}}},
	}}}

	for _, othersDown := range []bool{false, true} {
		_, err := engine.Cluster().ForceQuorum(context.Background(), requests.ClusterForceQuorum{OthersDown: othersDown})

		var failure command.Failure
		if !errors.As(err, &failure) || failure.Code != enums.ErrorCodeBusy {
			t.Errorf("othersDown=%v: err = %v", othersDown, err)
		}
	}
}

func TestRejoinResyncsOnlyWhenStale(t *testing.T) {
	stale := replStatus{Members: []memberStatus{{Name: "10.0.0.2:25060", Self: true, StateStr: "RECOVERING", InfoMessage: "error RS102 too stale to catch up"}}}
	healthy := replStatus{Members: []memberStatus{{Name: "10.0.0.2:25060", Self: true, StateStr: "SECONDARY"}}}

	if !stale.tooStale() || healthy.tooStale() {
		t.Fatal("only a member that fell off the oplog is resynced")
	}
}

func TestReadOnlyMembersLoseTheirVote(t *testing.T) {
	config := setConfig(
		bson.M{"_id": int64(0), "host": "10.0.0.1:25060", "votes": int64(1), "priority": float64(1)},
		bson.M{"_id": int64(1), "host": "10.0.0.2:25060", "votes": int64(1), "priority": float64(1)},
	)

	demoted, changed := withoutVote(config, "10.0.0.2:25060")

	member := demoted.member("10.0.0.2:25060")
	if !changed || member["votes"] != 0 || member["priority"] != 0 || demoted.voters() != 1 {
		t.Fatalf("config = %+v", demoted)
	}
	if _, again := withoutVote(demoted, "10.0.0.2:25060"); again {
		t.Fatal("a member without a vote was demoted twice")
	}
}

func TestDisableOnASecondaryLeavesThroughThePrimary(t *testing.T) {
	members := setConfig(
		bson.M{"_id": int64(0), "host": "10.0.0.1:25060", "votes": int64(1), "priority": float64(1)},
		bson.M{"_id": int64(1), "host": "10.0.0.2:25060", "votes": int64(1), "priority": float64(1)},
		bson.M{"_id": int64(2), "host": "10.0.0.3:25060", "votes": int64(0), "priority": float64(0)},
	)
	primarySent, localSent := []bson.D{}, []bson.D{}
	primary := fakeAdmin{replies: map[string]bson.M{"hello": {"isWritablePrimary": true}, "replSetGetConfig": {"config": bson.M(members)}}, sent: &primarySent}
	secondary := fakeAdmin{replies: map[string]bson.M{"hello": {"secondary": true}}}
	local := fakeAdmin{replies: map[string]bson.M{"replSetGetConfig": {"config": bson.M(members)}}, sent: &localSent}
	engine := Engine{admin: local, root: nodefs.Root(t.TempDir()), peers: func(host string) Admin {
		if host == "10.0.0.1:25060" {
			return primary
		}

		return secondary
	}}
	saved, _ := json.Marshal(requests.NodeConfigure{Ip: "10.0.0.3", Clustered: true, Members: []dtos.Member{{Ip: "10.0.0.1", Voting: true}, {Ip: "10.0.0.2", Voting: true}, {Ip: "10.0.0.3", Voting: true}}})
	if err := engine.root.Dir("state"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.root.Write(nodeState, string(saved), 0o600, false); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.Cluster().Disable(context.Background()); err != nil {
		t.Fatal(err)
	}

	removed := setConfiguration(primarySent[len(primarySent)-1][0].Value.(bson.M))
	alone := setConfiguration(localSent[len(localSent)-1][0].Value.(bson.M))
	if removed.member("10.0.0.3:25060") != nil || len(removed.members()) != 2 || len(alone.members()) != 1 || alone.member("10.0.0.3:25060") == nil || alone.member("10.0.0.3:25060")["votes"] != int64(1) || alone.member("10.0.0.3:25060")["priority"] != float64(1) || localSent[len(localSent)-1][1].Value != true {
		t.Fatalf("primary got %v, local got %v", primarySent, localSent)
	}
}

func TestDisableFinishesWhenThePrimaryAlreadyDroppedTheNode(t *testing.T) {
	remaining := setConfig(
		bson.M{"_id": int64(0), "host": "10.0.0.1:25060", "votes": int64(1), "priority": float64(1)},
		bson.M{"_id": int64(1), "host": "10.0.0.2:25060", "votes": int64(1), "priority": float64(1)},
	)
	primarySent, localSent := []bson.D{}, []bson.D{}
	primary := fakeAdmin{replies: map[string]bson.M{"hello": {"isWritablePrimary": true}, "replSetGetConfig": {"config": bson.M(remaining)}}, sent: &primarySent}
	local := fakeAdmin{replies: map[string]bson.M{"replSetGetConfig": {"config": bson.M(remaining)}}, sent: &localSent}
	engine := Engine{admin: local, root: nodefs.Root(t.TempDir()), peers: func(host string) Admin {
		if host == "10.0.0.1:25060" {
			return primary
		}

		return fakeAdmin{err: command.Unavailable("removed")}
	}}
	saved, _ := json.Marshal(requests.NodeConfigure{Ip: "10.0.0.3", Clustered: true, Members: []dtos.Member{{Ip: "10.0.0.1", Voting: true}, {Ip: "10.0.0.2", Voting: true}, {Ip: "10.0.0.3", Voting: true}}})
	if err := engine.root.Dir("state"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.root.Write(nodeState, string(saved), 0o600, false); err != nil {
		t.Fatal(err)
	}

	if _, err := engine.Cluster().Disable(context.Background()); err != nil {
		t.Fatal(err)
	}

	alone := setConfiguration(localSent[len(localSent)-1][0].Value.(bson.M))
	if slices.ContainsFunc(primarySent, func(cmd bson.D) bool { return cmd[0].Key == "replSetReconfig" }) || len(alone.members()) != 1 || alone.member("10.0.0.3:25060")["votes"] != int64(1) {
		t.Fatalf("primary got %v, local got %v", primarySent, localSent)
	}
}
