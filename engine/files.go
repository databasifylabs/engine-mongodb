package mongodb

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	family "github.com/databasifylabs/engine-mysql-family"
	"github.com/databasifylabs/engine-sdk/manifest"
	"github.com/databasifylabs/engine-sdk/requests"
)

const (
	clientPort    = 25060
	replicaSet    = "databasify"
	serverUnit    = "mongod"
	engineUser    = "mongodb"
	mongodConf    = "conf/mongod.conf"
	keyfilePath   = "conf/keyfile"
	adminPassword = "conf/admin.pw"
	manifestFile  = "conf/processes.json"
	stopTimeout   = 15 * time.Minute
	restartPause  = 3 * time.Second
)

func keyfileOf(replication string) string {
	mac := hmac.New(sha256.New, []byte(replication))
	mac.Write([]byte("mongodb keyfile"))

	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func numberOr(value string, fallback string) json.Number {
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return json.Number(value)
	}

	return json.Number(fallback)
}

func mongodConfOf(root string, effective map[string]string) string {
	defaults := map[string]string{}
	for _, field := range catalog().Fields {
		defaults[field.Key] = field.Default
	}
	value := func(key string) json.Number { return numberOr(effective[key], defaults[key]) }
	conf := map[string]any{
		"systemLog":          map[string]any{"verbosity": value(keyVerbosity)},
		"operationProfiling": map[string]any{"mode": "slowOp", "slowOpThresholdMs": value(keySlowOperation)},
		"setParameter":       map[string]any{keyTransactions: value(keyTransactions)},
		"net": map[string]any{
			"port": clientPort, "bindIpAll": true, "maxIncomingConnections": value(keyConnections),
			"tls": map[string]any{
				"mode": "requireTLS", "certificateKeyFile": root + "/tls/server.pem", "CAFile": root + "/tls/ca.crt",
				"allowConnectionsWithoutCertificates": true, "disabledProtocols": "TLS1_0,TLS1_1",
			},
		},
		"storage":     map[string]any{"dbPath": root + "/data", "wiredTiger": map[string]any{"engineConfig": map[string]any{keyCacheSize: value(keyCacheSize)}}},
		"security":    map[string]any{"keyFile": root + "/" + keyfilePath, "authorization": "enabled"},
		"replication": map[string]any{"replSetName": replicaSet},
	}
	encoded, _ := json.MarshalIndent(conf, "", "  ")

	return string(encoded) + "\n"
}

func manifestOf(root string, request requests.NodeConfigure) ([]byte, error) {
	server := manifest.Process{
		Name: serverUnit, User: engineUser, Argv: []string{"mongod", "--config", root + "/" + mongodConf},
		Restart: manifest.RestartAlways, RestartSec: manifest.Duration(restartPause), StopSignal: "TERM", StopTimeout: manifest.Duration(stopTimeout), Enabled: true,
	}

	processes, schedules := []manifest.Process{server}, []manifest.Schedule{}
	if request.Archive != nil && request.Secrets.Agent != nil {
		processes = append(processes, manifest.Process{
			Name: agentUnit, Argv: []string{"pbm-agent"}, User: engineUser, After: []string{serverUnit},
			Env:     map[string]string{"PBM_MONGODB_URI": pbmURIOf(*request.Secrets.Agent)},
			Restart: manifest.RestartAlways, RestartSec: manifest.Duration(restartPause), StopSignal: "TERM", StopTimeout: manifest.Duration(time.Minute), Enabled: true,
		})
		schedules = append(schedules, manifest.Schedule{Name: backupUnit, OnCalendar: fmt.Sprintf("%02d:00", request.Archive.BackupHour), Argv: []string{family.WrapperPath, "engine-task", "backup"}, Timeout: manifest.Duration(24 * time.Hour)})
	}

	return json.MarshalIndent(manifest.Manifest{Processes: processes, Schedules: schedules}, "", "  ")
}
