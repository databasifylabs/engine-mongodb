package mongodb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	family "github.com/databasifylabs/engine-mysql-family"
	"github.com/databasifylabs/engine-sdk/command"
	"github.com/databasifylabs/engine-sdk/contract"
	"github.com/databasifylabs/engine-sdk/dtos"
	"github.com/databasifylabs/engine-sdk/enums"
	"github.com/databasifylabs/engine-sdk/requests"
	"github.com/databasifylabs/engine-sdk/responses"
	"github.com/databasifylabs/engine-sdk/units"
)

const (
	agentUnit     = "pbm-agent"
	backupUnit    = "mongodb-backup"
	pbmConfig     = "conf/pbm.yaml"
	restoreLayout = "2006-01-02T15:04:05"
	resyncTimeout = 10 * time.Minute
	oplogSpanMin  = 10
)

type PbmRunner func(ctx context.Context, uri string, args ...string) (string, error)

func execPbm(ctx context.Context, uri string, args ...string) (string, error) {
	pbm := exec.CommandContext(ctx, "pbm", args...)
	pbm.Env = append(os.Environ(), "PBM_MONGODB_URI="+uri)
	output, err := pbm.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("pbm %s: %w: %s", strings.Join(args, " "), err, family.LastLines(string(output), 3))
	}

	return string(output), nil
}

func pbmURIOf(password string) string {
	credentials := url.UserPassword(pbmUser, password).String()

	return "mongodb://" + credentials + "@127.0.0.1:" + strconv.Itoa(clientPort) + "/?authSource=admin&tls=true&tlsInsecure=true"
}

func pbmConfigOf(storage dtos.Storage) string {
	config := map[string]any{
		"storage": map[string]any{
			"type": "s3",
			"s3": map[string]any{
				"region": storage.Region, "bucket": storage.Bucket, "prefix": storage.Path, "endpointUrl": storage.Endpoint,
				"forcePathStyle": storage.PathStyle, "insecureSkipTLSVerify": !storage.VerifyTls,
				"credentials": map[string]string{"access-key-id": storage.AccessKey, "secret-access-key": storage.SecretKey},
			},
		},
		"pitr":   map[string]any{"enabled": true, "oplogSpanMin": oplogSpanMin},
		"backup": map[string]any{"compression": "zstd"},
	}
	encoded, _ := json.MarshalIndent(config, "", "  ")

	return string(encoded) + "\n"
}

type archive struct{ Engine }

func (e Engine) Archive() contract.Archive { return archive{e} }

func (e Engine) agentURI() (string, error) {
	request, err := e.saved()
	if err != nil {
		return "", err
	}
	if request.Secrets.Agent == nil {
		return "", contract.ErrUnsupported
	}

	return pbmURIOf(*request.Secrets.Agent), nil
}

func (e Engine) pbmCommand(ctx context.Context, args ...string) (string, error) {
	uri, err := e.agentURI()
	if err != nil {
		return "", err
	}

	return e.pbm(ctx, uri, args...)
}

func (e Engine) usePbmStorage(ctx context.Context, storage dtos.Storage) error {
	if _, err := e.root.Write(pbmConfig, pbmConfigOf(storage), 0o600, false); err != nil {
		return err
	}
	_, err := e.pbmCommand(ctx, "config", "--file", e.root.Path(pbmConfig))

	return err
}

func (e Engine) writable(ctx context.Context) bool {
	state, err := e.anonymousHello(ctx)

	return err == nil && state.IsWritablePrimary
}

func (a archive) Backup(ctx context.Context) error {
	request, err := a.saved()
	if err != nil || request.Archive == nil || !a.writable(ctx) {
		return err
	}
	if err := a.usePbmStorage(ctx, request.Archive.Storage); err != nil {
		return err
	}
	if _, err := a.pbmCommand(ctx, "backup", "--compression=zstd", "--wait"); err != nil {
		return err
	}
	_, err = a.pbmCommand(ctx, "cleanup", "--older-than="+strconv.FormatInt(request.Archive.RetentionDays, 10)+"d", "--yes")

	return err
}

func (a archive) Stream(context.Context) error { return nil }

func (a archive) Ship(context.Context) error { return nil }

type pbmStatus struct {
	Pitr struct {
		Error string `json:"error"`
	} `json:"pitr"`
	Running struct {
		Type string `json:"type"`
	} `json:"running"`
	Backups struct {
		Snapshot []struct {
			Name       string `json:"name"`
			Status     string `json:"status"`
			RestoreTo  int64  `json:"restoreTo"`
			CompleteTS int64  `json:"completeTS"`
		} `json:"snapshot"`
		PitrChunks struct {
			PitrChunks []struct {
				Range struct {
					Start int64 `json:"start"`
					End   int64 `json:"end"`
				} `json:"range"`
			} `json:"pitrChunks"`
		} `json:"pitrChunks"`
	} `json:"backups"`
}

func statusOf(raw string) (responses.ArchiveStatus, error) {
	var parsed pbmStatus
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return responses.ArchiveStatus{}, fmt.Errorf("pbm status is not JSON: %w", err)
	}
	status := responses.ArchiveStatus{}
	var earliest, newest time.Time
	for _, snapshot := range parsed.Backups.Snapshot {
		if snapshot.Status != "done" {
			continue
		}
		status.Backups++
		done := time.Unix(max(snapshot.RestoreTo, snapshot.CompleteTS), 0).UTC()
		if earliest.IsZero() || done.Before(earliest) {
			earliest = done
		}
		if done.After(newest) {
			newest = done
		}
	}
	if status.Backups == 0 {
		return status, nil
	}
	status.Earliest, status.LastBackup = &earliest, &newest
	latest := newest
	for _, chunk := range parsed.Backups.PitrChunks.PitrChunks {
		if end := time.Unix(chunk.Range.End, 0).UTC(); end.After(latest) {
			latest = end
		}
	}
	status.Latest = &latest
	if parsed.Pitr.Error != "" {
		problem := parsed.Pitr.Error
		status.Problem = &problem
	}

	return status, nil
}

func (a archive) Status(ctx context.Context, request requests.ArchiveStatus) (responses.ArchiveStatus, error) {
	saved, err := a.saved()
	if err != nil {
		return responses.ArchiveStatus{}, err
	}
	if saved.Archive == nil {
		return responses.ArchiveStatus{}, contract.ErrUnsupported
	}
	if a.writable(ctx) {
		if err := a.usePbmStorage(ctx, saved.Archive.Storage); err != nil {
			return responses.ArchiveStatus{}, err
		}
	}
	raw, err := a.pbmCommand(ctx, "status", "--out=json")
	if err != nil {
		return responses.ArchiveStatus{}, err
	}
	status, err := statusOf(raw)
	if err != nil {
		return responses.ArchiveStatus{}, err
	}
	if units.Failed(a.supervisor.Property(backupUnit, "Result")) {
		problem := "The last backup failed, see the " + backupUnit + " log."
		status.Problem = &problem
	}
	if request.BackUpNow || status.Backups == 0 && status.Problem == nil {
		_ = a.supervisor.DoNoBlock("start", backupUnit)
	}

	return status, nil
}

func (a archive) Restore(ctx context.Context, request requests.RestoreStart, report contract.Report) error {
	saved, err := a.saved()
	if err != nil {
		return err
	}
	if saved.Archive == nil {
		return command.Failure{Code: enums.ErrorCodeBusy, Message: "configure archive on this database before restoring into it"}
	}
	report("stopping oplog slicing")
	if err := a.syncStorage(ctx, request.Storage); err != nil {
		return err
	}
	if _, err := a.pbmCommand(ctx, "config", "--set", "pitr.enabled=false"); err != nil {
		return err
	}
	report("restoring")
	if _, err := a.pbmCommand(ctx, "restore", "--time="+request.Target.UTC().Format(restoreLayout), "--yes", "--wait"); err != nil {
		return err
	}
	if err := a.reclaimInternalUsers(ctx, saved.Secrets); err != nil {
		return err
	}
	report("backing up")
	if err := a.syncStorage(ctx, saved.Archive.Storage); err != nil {
		return err
	}
	if _, err := a.pbmCommand(ctx, "config", "--set", "pitr.enabled=true"); err != nil {
		return err
	}
	_, err = a.pbmCommand(ctx, "backup", "--compression=zstd", "--wait")

	return err
}

func (a archive) syncStorage(ctx context.Context, storage dtos.Storage) error {
	if err := a.usePbmStorage(ctx, storage); err != nil {
		return err
	}
	started := time.Now().Unix()
	if _, err := a.pbmCommand(ctx, "config", "--force-resync"); err != nil {
		return err
	}

	return family.Until(ctx, resyncTimeout, func() (bool, error) { return a.resynced(ctx, started) })
}

type pbmLogEntry struct {
	Time     int64  `json:"ts"`
	Severity int    `json:"s"`
	Message  string `json:"msg"`
}

const pbmErrorSeverity = 1

func (a archive) resynced(ctx context.Context, since int64) (bool, error) {
	raw, err := a.pbmCommand(ctx, "logs", "--event=resync", "--tail=20", "--out=json")
	if err != nil {
		return false, nil
	}
	var entries []pbmLogEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return false, nil
	}
	succeeded := false
	for _, entry := range entries {
		if entry.Time < since {
			continue
		}
		if entry.Severity <= pbmErrorSeverity {
			return false, fmt.Errorf("PBM could not read the backup storage: %s", entry.Message)
		}
		succeeded = succeeded || entry.Message == "succeed"
	}
	if !succeeded {
		return false, nil
	}
	raw, err = a.pbmCommand(ctx, "status", "--out=json")
	var status pbmStatus

	return err == nil && json.Unmarshal([]byte(raw), &status) == nil && status.Running.Type == "", nil
}

func (a archive) reclaimInternalUsers(ctx context.Context, secrets dtos.Secrets) error {
	if secrets.Admin == nil || secrets.Agent == nil {
		return nil
	}
	member := a.Engine
	member.admin, member.anonymous = a.member, a.member
	if err := member.ensureRoot(ctx, *secrets.Admin); err != nil {
		return err
	}

	return member.ensureAgent(ctx, *secrets.Agent)
}
