package mongodb

import (
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	family "github.com/databasifylabs/engine-mysql-family"
	"github.com/databasifylabs/engine-sdk/command"
	"github.com/databasifylabs/engine-sdk/contract"
	"github.com/databasifylabs/engine-sdk/enums"
	"github.com/databasifylabs/engine-sdk/requests"
	"github.com/databasifylabs/engine-sdk/responses"
)

const (
	maxVoters      = 7
	rejoinTimeout  = 10 * time.Minute
	stepDownWindow = 60
	catchUpSeconds = 10
)

type setConfiguration bson.M

type memberStatus struct {
	Name        string    `bson:"name"`
	Health      float64   `bson:"health"`
	StateStr    string    `bson:"stateStr"`
	Self        bool      `bson:"self"`
	InfoMessage string    `bson:"infoMessage"`
	OptimeDate  time.Time `bson:"optimeDate"`
}

type replStatus struct {
	Members []memberStatus `bson:"members"`
}

func numberOf(value any) int64 {
	switch typed := value.(type) {
	case int32:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	}

	return 0
}

func (c setConfiguration) members() []bson.M {
	switch list := c["members"].(type) {
	case []bson.M:
		return list
	case bson.A:
		members := []bson.M{}
		for _, item := range list {
			if member, ok := item.(bson.M); ok {
				members = append(members, member)
			}
			if member, ok := item.(bson.D); ok {
				members = append(members, bsonMapOf(member))
			}
		}

		return members
	}

	return nil
}

func bsonMapOf(document bson.D) bson.M {
	mapped := bson.M{}
	for _, element := range document {
		mapped[element.Key] = element.Value
	}

	return mapped
}

func (c setConfiguration) member(host string) bson.M {
	for _, member := range c.members() {
		if member["host"] == host {
			return member
		}
	}

	return nil
}

func (c setConfiguration) with(members []bson.M) setConfiguration {
	next := setConfiguration{}
	for key, value := range c {
		next[key] = value
	}
	next["members"] = members
	next["version"] = numberOf(c["version"]) + 1

	return next
}

func (c setConfiguration) voters() int {
	count := 0
	for _, member := range c.members() {
		if _, set := member["votes"]; !set || numberOf(member["votes"]) > 0 {
			count++
		}
	}

	return count
}

func withMember(config setConfiguration, host string) (setConfiguration, bool) {
	if config.member(host) != nil {
		return config, false
	}
	next := int64(0)
	for _, member := range config.members() {
		next = max(next, numberOf(member["_id"])+1)
	}
	members := append(slices.Clone(config.members()), bson.M{"_id": next, "host": host, "votes": 0, "priority": 0})

	return config.with(members), true
}

func withVote(config setConfiguration, host string, secondary bool) (setConfiguration, bool) {
	member := config.member(host)
	if member == nil || !secondary || numberOf(member["votes"]) > 0 || config.voters() >= maxVoters {
		return config, false
	}
	members := []bson.M{}
	for _, existing := range config.members() {
		if existing["host"] == host {
			existing = bson.M{"_id": existing["_id"], "host": host, "votes": 1, "priority": 1}
		}
		members = append(members, existing)
	}

	return config.with(members), true
}

func withoutVote(config setConfiguration, host string) (setConfiguration, bool) {
	member := config.member(host)
	if member == nil || numberOf(member["votes"]) == 0 {
		return config, false
	}
	members := []bson.M{}
	for _, existing := range config.members() {
		if existing["host"] == host {
			existing = bson.M{"_id": existing["_id"], "host": host, "votes": 0, "priority": 0}
		}
		members = append(members, existing)
	}

	return config.with(members), true
}

func (s replStatus) self() memberStatus {
	for _, member := range s.Members {
		if member.Self {
			return member
		}
	}

	return memberStatus{}
}

func (s replStatus) tooStale() bool {
	self := s.self()

	return self.StateStr == "RECOVERING" && (strings.Contains(self.InfoMessage, "RS102") || strings.Contains(strings.ToLower(self.InfoMessage), "too stale"))
}

func (s replStatus) hasPrimary() bool {
	return slices.ContainsFunc(s.Members, func(member memberStatus) bool { return member.StateStr == "PRIMARY" })
}

type cluster struct{ Engine }

func (e Engine) Cluster() contract.Cluster { return cluster{e} }

func (e Engine) peer(host string) Admin {
	return e.peers(host)
}

func (e Engine) config(ctx context.Context, admin Admin) (setConfiguration, error) {
	var reply struct {
		Config setConfiguration `bson:"config"`
	}

	return reply.Config, admin.Run(ctx, "admin", bson.D{{Key: "replSetGetConfig", Value: 1}}, &reply)
}

func (e Engine) reconfig(ctx context.Context, admin Admin, config setConfiguration, force bool) error {
	return admin.Run(ctx, "admin", bson.D{{Key: "replSetReconfig", Value: bson.M(config)}, {Key: "force", Value: force}}, nil)
}

func (e Engine) status(ctx context.Context) (replStatus, error) {
	var status replStatus

	return status, e.admin.Run(ctx, "admin", bson.D{{Key: "replSetGetStatus", Value: 1}}, &status)
}

func (e Engine) primaryOf(ctx context.Context, request requests.NodeConfigure) (Admin, string, error) {
	for _, member := range request.Members {
		host := hostOf(member.Ip)
		var state hello
		if err := e.peer(host).Run(ctx, "admin", bson.D{{Key: "hello", Value: 1}}, &state); err == nil && state.IsWritablePrimary {
			return e.peer(host), host, nil
		}
	}

	return nil, "", command.Unavailable("no member of the replica set is primary yet")
}

func (e cluster) Enable(ctx context.Context, _ requests.ClusterEnable, report contract.Report) error {
	request, err := e.saved()
	if err != nil {
		return err
	}
	if request.Seed() {
		report("bootstrapping")

		return e.initiate(ctx, request)
	}
	report("joining")

	return e.join(ctx, request, report)
}

func (e Engine) join(ctx context.Context, request requests.NodeConfigure, report contract.Report) error {
	host := hostOf(request.Ip)
	if err := family.Retry(ctx, readyTimeout, func() error { return e.addSelf(ctx, request, host) }); err != nil {
		return err
	}
	report("syncing")
	if err := e.waitSynced(ctx, request); err != nil {
		return err
	}
	if request.ReadOnly {
		return nil
	}
	report("voting")

	return family.Retry(ctx, rejoinTimeout, func() error { return e.voteSelf(ctx, request, host) })
}

func (e Engine) waitSynced(ctx context.Context, request requests.NodeConfigure) error {
	err := family.Until(ctx, readyTimeout, func() (bool, error) {
		state, err := e.anonymousHello(ctx)
		if err == nil && (state.Secondary || state.IsWritablePrimary) {
			return true, nil
		}
		if status, err := e.status(ctx); err == nil && status.tooStale() {
			return false, errStale
		}

		return false, nil
	})
	if !errors.Is(err, errStale) {
		return err
	}

	return e.resync(ctx, request)
}

func (e Engine) addSelf(ctx context.Context, request requests.NodeConfigure, host string) error {
	primary, _, err := e.primaryOf(ctx, request)
	if err != nil {
		return err
	}
	config, err := e.config(ctx, primary)
	if err != nil {
		return err
	}
	next, changed := withMember(config, host)
	if !changed {
		return nil
	}

	return e.reconfig(ctx, primary, next, false)
}

func (e Engine) voteSelf(ctx context.Context, request requests.NodeConfigure, host string) error {
	primary, _, err := e.primaryOf(ctx, request)
	if err != nil {
		return err
	}
	config, err := e.config(ctx, primary)
	if err != nil {
		return err
	}
	state, err := e.anonymousHello(ctx)
	if err != nil {
		return err
	}
	next, changed := withVote(config, host, state.Secondary)
	if !changed {
		return nil
	}

	return e.reconfig(ctx, primary, next, false)
}

func (e Engine) waitSecondary(ctx context.Context, timeout time.Duration) error {
	return family.Until(ctx, timeout, func() (bool, error) {
		state, err := e.anonymousHello(ctx)

		return err == nil && (state.Secondary || state.IsWritablePrimary), nil
	})
}

func (e cluster) Disable(ctx context.Context) (responses.ClusterDisable, error) {
	request, err := e.saved()
	if err != nil {
		return responses.ClusterDisable{}, err
	}
	config, err := e.config(ctx, e.admin)
	if err != nil {
		return responses.ClusterDisable{}, err
	}
	host := hostOf(request.Ip)
	self := config.member(host)
	if self == nil {
		self = bson.M{"_id": int64(0), "host": host}
	}
	primary, current, err := e.primaryOf(ctx, request)
	if err != nil {
		return responses.ClusterDisable{}, err
	}
	alone := config.with([]bson.M{votingAlone(self)})
	if current == host {
		return responses.ClusterDisable{}, e.reconfig(ctx, e.admin, alone, false)
	}
	if err := e.removeThrough(ctx, primary, host); err != nil {
		return responses.ClusterDisable{}, err
	}

	return responses.ClusterDisable{}, e.reconfig(ctx, e.admin, alone, true)
}

func votingAlone(member bson.M) bson.M {
	alone := maps.Clone(member)
	alone["votes"], alone["priority"] = int64(1), float64(1)
	delete(alone, "hidden")

	return alone
}

func (e Engine) removeThrough(ctx context.Context, primary Admin, host string) error {
	config, err := e.config(ctx, primary)
	if err != nil {
		return err
	}
	if config.member(host) == nil {
		return nil
	}
	kept := slices.DeleteFunc(slices.Clone(config.members()), func(member bson.M) bool { return member["host"] == host })

	return e.reconfig(ctx, primary, config.with(kept), false)
}

func (e cluster) Failover(ctx context.Context, request requests.ClusterFailover) error {
	saved, err := e.saved()
	if err != nil {
		return err
	}
	primary, current, err := e.primaryOf(ctx, saved)
	if err != nil {
		return err
	}
	target := hostOf(request.Target)
	if current == target {
		return nil
	}
	config, err := e.config(ctx, primary)
	if err != nil {
		return err
	}
	if config.member(target) == nil {
		return command.NotFound(request.Target + " is not a member of the replica set")
	}
	if err := e.reconfig(ctx, primary, withPriority(config, target, 2), false); err != nil {
		return err
	}
	err = primary.Run(ctx, "admin", bson.D{{Key: "replSetStepDown", Value: stepDownWindow}, {Key: "secondaryCatchUpPeriodSecs", Value: catchUpSeconds}}, nil)
	if err != nil && !isUnavailable(err) {
		return err
	}
	promoted := e.peer(target)
	if err := family.Until(ctx, rejoinTimeout, func() (bool, error) {
		var state hello
		err := promoted.Run(ctx, "admin", bson.D{{Key: "hello", Value: 1}}, &state)

		return err == nil && state.IsWritablePrimary, nil
	}); err != nil {
		return err
	}
	settled, err := e.config(ctx, promoted)
	if err != nil {
		return err
	}

	return e.reconfig(ctx, promoted, withPriority(settled, target, 1), false)
}

func withPriority(config setConfiguration, host string, priority int) setConfiguration {
	members := []bson.M{}
	for _, member := range config.members() {
		if member["host"] == host {
			member["priority"] = priority
		}
		members = append(members, member)
	}

	return config.with(members)
}

func (e cluster) Quorum(ctx context.Context) (responses.ClusterQuorum, error) {
	config, err := e.config(ctx, e.admin)
	if err != nil {
		return responses.ClusterQuorum{}, nil
	}
	quorum := responses.ClusterQuorum{Total: int64(config.voters())}
	status, err := e.status(ctx)
	if err != nil || !status.hasPrimary() {
		return quorum, nil
	}
	quorum.Online = int64(healthyVoters(config, status))

	return quorum, nil
}

func healthyVoters(config setConfiguration, status replStatus) int {
	count := 0
	for _, member := range status.Members {
		voter := config.member(member.Name)
		if member.Health == 1 && voter != nil && (voter["votes"] == nil || numberOf(voter["votes"]) > 0) {
			count++
		}
	}

	return count
}

func (e cluster) ForceQuorum(ctx context.Context, request requests.ClusterForceQuorum) (responses.ClusterForceQuorum, error) {
	busy := command.Failure{Code: enums.ErrorCodeBusy, Message: "forcing the replica set while other members may be alive would roll back writes; confirm the others are down"}
	if !request.OthersDown {
		return responses.ClusterForceQuorum{}, busy
	}
	config, err := e.config(ctx, e.admin)
	if err != nil {
		return responses.ClusterForceQuorum{}, err
	}
	status, err := e.status(ctx)
	if err != nil {
		return responses.ClusterForceQuorum{}, err
	}
	if healthyVoters(config, status)*2 > config.voters() {
		return responses.ClusterForceQuorum{}, command.Failure{Code: enums.ErrorCodeBusy, Message: "a majority of voting members is healthy; the replica set elects a primary on its own"}
	}
	healthy := []bson.M{}
	for _, member := range status.Members {
		if member.Health == 1 {
			if kept := config.member(member.Name); kept != nil {
				healthy = append(healthy, kept)
			}
		}
	}
	if err := e.reconfig(ctx, e.admin, config.with(healthy), true); err != nil {
		return responses.ClusterForceQuorum{}, err
	}
	saved, err := e.saved()
	if err != nil {
		return responses.ClusterForceQuorum{}, err
	}

	return responses.ClusterForceQuorum{Leader: saved.Ip}, nil
}

func (e cluster) Position(ctx context.Context) (responses.ClusterPosition, error) {
	position := int64(-1)
	if e.supervisor.Active(serverUnit) {
		if status, err := e.status(ctx); err == nil {
			position = status.self().OptimeDate.Unix()
		}
	}

	return responses.ClusterPosition{Position: &position}, nil
}

func (e cluster) Rejoin(ctx context.Context, _ requests.ClusterRejoin, report contract.Report) error {
	request, err := e.saved()
	if err != nil {
		return err
	}
	report("starting MongoDB")
	if err := e.supervisor.Do("start", serverUnit); err != nil {
		return err
	}

	return e.join(ctx, request, report)
}

var errStale = errors.New("this member fell off the oplog")

func (e Engine) resync(ctx context.Context, request requests.NodeConfigure) error {
	if _, _, err := e.primaryOf(ctx, request); err != nil {
		return command.Failure{Code: enums.ErrorCodeBusy, Message: "this member is too stale to catch up and no primary is reachable to copy from; its data stays untouched"}
	}
	if err := e.supervisor.Do("stop", serverUnit); err != nil {
		return err
	}
	entries, err := os.ReadDir(e.root.Path("data"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(e.root.Path("data/" + entry.Name())); err != nil {
			return err
		}
	}
	if err := e.supervisor.Do("start", serverUnit); err != nil {
		return err
	}

	return e.waitSecondary(ctx, readyTimeout)
}

func (e cluster) UpdateMembership(ctx context.Context, request requests.ClusterMembership) error {
	saved, err := e.saved()
	if err != nil {
		return err
	}
	primary, _, err := e.primaryOf(ctx, saved)
	if err != nil {
		return err
	}
	for _, member := range request.Members {
		if member.Voting {
			continue
		}
		config, err := e.config(ctx, primary)
		if err != nil {
			return err
		}
		if next, changed := withoutVote(config, hostOf(member.Ip)); changed {
			if err := e.reconfig(ctx, primary, next, false); err != nil {
				return err
			}
		}
	}
	config, err := e.config(ctx, primary)
	if err != nil {
		return err
	}
	wanted := []string{}
	for _, member := range request.Members {
		wanted = append(wanted, hostOf(member.Ip))
	}
	kept := slices.DeleteFunc(slices.Clone(config.members()), func(member bson.M) bool { return !slices.Contains(wanted, member["host"].(string)) })
	if len(kept) == len(config.members()) {
		return nil
	}

	return e.reconfig(ctx, primary, config.with(kept), false)
}

func (e cluster) Admit(context.Context, requests.MemberAdmit) error { return nil }

func (e cluster) Promote(context.Context, requests.MemberPromote) error { return nil }

func (e cluster) Forget(ctx context.Context, request requests.MemberForget) error {
	saved, err := e.saved()
	if err != nil {
		return err
	}
	primary, _, err := e.primaryOf(ctx, saved)
	if err != nil {
		return err
	}

	return e.removeThrough(ctx, primary, hostOf(request.Ip))
}

func (e cluster) Watch(context.Context) error { return contract.ErrUnsupported }
