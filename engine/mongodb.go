package mongodb

import (
	"os"

	"github.com/databasifylabs/engine-sdk/contract"
	"github.com/databasifylabs/engine-sdk/ctl"
	"github.com/databasifylabs/engine-sdk/dtos"
	"github.com/databasifylabs/engine-sdk/layout"
	"github.com/databasifylabs/engine-sdk/nodefs"
	"github.com/databasifylabs/engine-sdk/units"
)

const root = "/srv/mongodb"

type Supervisor interface {
	Do(verb string, units ...string) error
	Run(transient ctl.Transient) error
	DoNoBlock(verb string, units ...string) error
	Kill(signal string, units ...string) error
	Active(unit string) bool
	Property(unit, property string) string
}

type Engine struct {
	admin      Admin
	anonymous  Admin
	member     Admin
	remote     func(uri string) Admin
	peers      func(host string) Admin
	pbm        PbmRunner
	supervisor Supervisor
	line       string
	root       nodefs.Root
}

func New() Engine {
	return Engine{
		admin:      localAdmin{passwordFile: root + "/" + adminPassword},
		anonymous:  localAdmin{anonymous: true},
		member:     localAdmin{passwordFile: root + "/" + keyfilePath, member: true},
		remote:     func(uri string) Admin { return remoteAdmin{uri: uri} },
		peers:      func(host string) Admin { return localAdmin{host: host, passwordFile: root + "/" + adminPassword} },
		pbm:        execPbm,
		supervisor: units.Control{Socket: layout.FromEnv().Socket()},
		line:       os.Getenv("DATABASIFY_LINE"),
		root:       root,
	}
}

func (Engine) Name() string { return "mongodb" }

func (Engine) Capabilities() contract.Capabilities {
	encryption := scramSha256

	return contract.Capabilities{
		PasswordEncryptions:       []string{encryption, "SCRAM-SHA-1"},
		DefaultPasswordEncryption: &encryption,
		ReservedUsers:             []string{"root", "databasify_pbm", "__system"},
		ReservedDatabases:         []string{"admin", "local", "config"},
		Ports:                     dtos.Ports{Client: clientPort, Peers: []int{}},
	}
}

func (Engine) HostProfile() contract.HostProfile {
	return contract.HostProfile{
		Settings: map[string]string{
			"vm.swappiness":                 "1",
			"net.core.somaxconn":            "4096",
			"net.ipv4.tcp_keepalive_time":   "120",
			"net.ipv4.tcp_keepalive_intvl":  "10",
			"net.ipv4.tcp_keepalive_probes": "6",
		},
		Floors: map[string]int64{"vm.max_map_count": 262144},
	}
}

type nodes struct{ Engine }

func (e Engine) Nodes() contract.Nodes { return nodes{e} }

func (Engine) Pools() contract.Pools { return contract.UnsupportedPools{} }
