package mongodb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	family "github.com/databasifylabs/engine-mysql-family"
	"github.com/databasifylabs/engine-sdk/contract"
	"github.com/databasifylabs/engine-sdk/dtos"
	"github.com/databasifylabs/engine-sdk/requests"
	"github.com/databasifylabs/engine-sdk/responses"
)

const (
	migrationUnit     = "mongodb-migration"
	migrationDir      = "migration"
	pcsmAddress       = "http://127.0.0.1:2242"
	pcsmDatabase      = "percona_clustersync_mongodb"
	pcsmStartTimeout  = time.Minute
	pcsmFinalizeLimit = time.Hour
)

type migration struct{ Engine }

func (e Engine) Migration() contract.Migration { return migration{e} }

func (e Engine) migrator() family.Migrator {
	return family.Migrator{Root: e.root, Supervisor: e.supervisor, Unit: migrationUnit}
}

func sourceURIOf(source dtos.MigrationSource, direct bool) string {
	address := url.URL{Scheme: "mongodb", User: url.UserPassword(source.Username, source.Password), Host: source.Host + ":" + strconv.FormatInt(source.Port, 10), Path: "/"}
	query := url.Values{"authSource": {"admin"}, "tls": {strconv.FormatBool(source.Tls)}}
	if source.Tls {
		query.Set("tlsInsecure", "true")
	}
	if direct {
		query.Set("directConnection", "true")
	}
	address.RawQuery = query.Encode()

	return address.String()
}

func (e Engine) localURI() string {
	password := localAdmin{passwordFile: e.root.Path(adminPassword)}.password()

	return "mongodb://" + url.UserPassword("root", password).String() + "@127.0.0.1:" + strconv.Itoa(clientPort) + "/?authSource=admin&tls=true&tlsInsecure=true"
}

func majorOf(version string) string {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return version
	}

	return parts[0] + "." + parts[1]
}

type sourceDatabases struct {
	Databases []struct {
		Name       string `bson:"name"`
		SizeOnDisk int64  `bson:"sizeOnDisk"`
	} `bson:"databases"`
}

func (e migration) Check(ctx context.Context, request requests.MigrationCheck) (responses.MigrationCheck, error) {
	report := responses.MigrationCheck{Databases: []string{}, Unreadable: []string{}, Unowned: []string{}, Occupied: []string{}, Warnings: []string{}}
	source := e.remote(sourceURIOf(request.Source, true))
	var build struct {
		Version string `bson:"version"`
	}
	if err := source.Run(ctx, "admin", bson.D{{Key: "buildInfo", Value: 1}}, &build); err != nil {
		message := strings.SplitN(err.Error(), "\n", 2)[0]
		report.Error = &message

		return report, nil
	}
	report.Reachable, report.Version = true, &build.Version
	var listed sourceDatabases
	if err := source.Run(ctx, "admin", bson.D{{Key: "listDatabases", Value: 1}}, &listed); err != nil {
		return responses.MigrationCheck{}, err
	}
	sizes := map[string]int64{}
	for _, database := range listed.Databases {
		if slices.Contains(e.Capabilities().ReservedDatabases, database.Name) {
			continue
		}
		report.Databases = append(report.Databases, database.Name)
		sizes[database.Name] = database.SizeOnDisk
	}
	wanted := request.Databases
	if len(wanted) == 0 {
		wanted = report.Databases
	}
	var size int64
	for _, database := range wanted {
		if !slices.Contains(report.Databases, database) {
			report.Unreadable = append(report.Unreadable, database)

			continue
		}
		size += sizes[database]
		if e.occupied(ctx, database) {
			report.Occupied = append(report.Occupied, database)
		}
	}
	megabytes := size / (1 << 20)
	report.SizeMb = &megabytes
	report.CanReplicate, report.Warnings = e.replicationReadiness(ctx, source, build.Version, report.Warnings)

	return report, nil
}

func (e Engine) replicationReadiness(ctx context.Context, source Admin, version string, warnings []string) (bool, []string) {
	var hello struct {
		SetName string `bson:"setName"`
	}
	_ = source.Run(ctx, "admin", bson.D{{Key: "hello", Value: 1}}, &hello)
	if hello.SetName == "" {
		warnings = append(warnings, "the source is a standalone server without an oplog; only a one-shot copy is possible")
	}
	sameMajor := majorOf(version) == e.line
	if !sameMajor {
		warnings = append(warnings, "continuous sync needs the same major version ("+e.line+"); only a one-shot copy is possible")
	}

	return hello.SetName != "" && sameMajor, warnings
}

func (e Engine) occupied(ctx context.Context, database string) bool {
	var listed struct {
		Cursor struct {
			FirstBatch []struct {
				Name string `bson:"name"`
			} `bson:"firstBatch"`
		} `bson:"cursor"`
	}
	if err := e.admin.Run(ctx, database, bson.D{{Key: "listCollections", Value: 1}, {Key: "nameOnly", Value: true}}, &listed); err != nil {
		return false
	}

	return slices.ContainsFunc(listed.Cursor.FirstBatch, func(collection struct {
		Name string `bson:"name"`
	}) bool {
		return collection.Name != placeholderCollection
	})
}

func (e migration) Start(_ context.Context, request requests.MigrationStart) error {
	return e.migrator().Start(request)
}

func (e migration) Copy(ctx context.Context) error {
	source, databases, continuous := family.SourceFromEnvironment()
	if len(databases) == 0 {
		listed, err := e.Check(ctx, requests.MigrationCheck{Source: source})
		if err != nil {
			return err
		}
		databases = listed.Databases
	}
	if continuous {
		return e.sync(ctx, source, databases)
	}

	return e.copyOnce(ctx, source, databases)
}

func namespacesOf(databases []string) []string {
	namespaces := make([]string, 0, len(databases))
	for _, database := range databases {
		namespaces = append(namespaces, database+".*")
	}

	return namespaces
}

func toolConfigOf(uri string) string {
	encoded, _ := json.Marshal(uri)

	return "uri: " + string(encoded) + "\n"
}

func dumpArguments(config string, database string) []string {
	return []string{"--config=" + config, "--archive", "--gzip", "--db=" + database}
}

func restoreArguments(config string, database string) []string {
	return []string{"--config=" + config, "--archive", "--gzip", "--nsInclude=" + database + ".*", "--drop", "--numInsertionWorkersPerCollection=4"}
}

func (e Engine) copyOnce(ctx context.Context, source dtos.MigrationSource, databases []string) error {
	if err := e.root.Dir(migrationDir); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(e.root.Path(migrationDir)) }()
	dumpConfig, restoreConfig := migrationDir+"/source.yaml", migrationDir+"/target.yaml"
	if _, err := e.root.Write(dumpConfig, toolConfigOf(sourceURIOf(source, false)), 0o600, false); err != nil {
		return err
	}
	if _, err := e.root.Write(restoreConfig, toolConfigOf(e.localURI()), 0o600, false); err != nil {
		return err
	}
	for _, database := range databases {
		if err := copyDatabase(ctx, e.root.Path(dumpConfig), e.root.Path(restoreConfig), database); err != nil {
			return err
		}
	}

	return nil
}

func copyDatabase(ctx context.Context, dumpConfig string, restoreConfig string, database string) error {
	dump := exec.CommandContext(ctx, "mongodump", dumpArguments(dumpConfig, database)...)
	restore := exec.CommandContext(ctx, "mongorestore", restoreArguments(restoreConfig, database)...)
	pipe, err := dump.StdoutPipe()
	if err != nil {
		return err
	}
	restore.Stdin, dump.Stderr, restore.Stdout, restore.Stderr = pipe, os.Stderr, os.Stdout, os.Stderr
	if err := restore.Start(); err != nil {
		return err
	}
	if err := dump.Run(); err != nil {
		_ = restore.Wait()

		return fmt.Errorf("mongodump could not read %s from the source: %w", database, err)
	}
	if err := restore.Wait(); err != nil {
		return fmt.Errorf("mongorestore could not write %s: %w", database, err)
	}

	return nil
}

func pcsmCommand(ctx context.Context, sourceURI string, targetURI string) *exec.Cmd {
	pcsm := exec.CommandContext(ctx, "pcsm", "--log-no-color")
	pcsm.Env = append(os.Environ(), "PCSM_SOURCE_URI="+sourceURI, "PCSM_TARGET_URI="+targetURI)
	pcsm.Stdout, pcsm.Stderr = os.Stdout, os.Stderr

	return pcsm
}

func (e Engine) sync(ctx context.Context, source dtos.MigrationSource, databases []string) error {
	pcsm := pcsmCommand(ctx, sourceURIOf(source, false), e.localURI())
	if err := pcsm.Start(); err != nil {
		return err
	}
	started := family.Until(ctx, pcsmStartTimeout, func() (bool, error) {
		_, err := pcsmStatusOf(ctx)

		return err == nil, nil
	})
	if started == nil {
		started = pcsmPost(ctx, "/start", map[string]any{"includeNamespaces": namespacesOf(databases)})
	}
	if started != nil {
		_ = pcsm.Process.Kill()
		_ = pcsm.Wait()

		return started
	}

	return pcsm.Wait()
}

type pcsmStatus struct {
	Ok             bool   `json:"ok"`
	Error          string `json:"error"`
	State          string `json:"state"`
	LagTimeSeconds int64  `json:"lagTimeSeconds"`
	InitialSync    *struct {
		Completed bool `json:"completed"`
	} `json:"initialSync"`
}

func (s pcsmStatus) inSync() bool {
	return s.State == "running" && s.InitialSync != nil && s.InitialSync.Completed
}

func pcsmStatusOf(ctx context.Context) (pcsmStatus, error) {
	var status pcsmStatus
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, pcsmAddress+"/status", nil)
	if err != nil {
		return status, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return status, err
	}
	defer response.Body.Close()

	return status, json.NewDecoder(response.Body).Decode(&status)
}

func pcsmPost(ctx context.Context, path string, body any) error {
	encoded, _ := json.Marshal(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, pcsmAddress+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	var reply pcsmStatus
	if err := json.NewDecoder(response.Body).Decode(&reply); err != nil {
		return err
	}
	if !reply.Ok {
		return fmt.Errorf("ClusterSync %s: %s", path, reply.Error)
	}

	return nil
}

func (e migration) Progress(ctx context.Context) (responses.MigrationProgress, error) {
	return e.migrator().Progress(func() (int64, bool) {
		status, err := pcsmStatusOf(ctx)
		if err != nil || !status.inSync() {
			return 0, false
		}

		return max(status.LagTimeSeconds, 0), true
	}), nil
}

func (e migration) Stop(ctx context.Context) error {
	if status, err := pcsmStatusOf(ctx); err == nil {
		if err := e.finishSync(ctx, status); err != nil {
			return err
		}
	}
	e.migrator().StopCopy()

	return e.admin.Run(ctx, pcsmDatabase, bson.D{{Key: "dropDatabase", Value: 1}}, nil)
}

func (e migration) finishSync(ctx context.Context, status pcsmStatus) error {
	if !status.inSync() {
		if status.State != "running" {
			return nil
		}

		return pcsmPost(ctx, "/pause", map[string]any{})
	}
	if err := pcsmPost(ctx, "/finalize", map[string]any{}); err != nil {
		return err
	}

	return family.Until(ctx, pcsmFinalizeLimit, func() (bool, error) {
		status, err := pcsmStatusOf(ctx)
		if err != nil {
			return false, err
		}
		if status.State == "failed" {
			return false, fmt.Errorf("ClusterSync could not finalize: %s", status.Error)
		}

		return status.State == "finalized", nil
	})
}
