package mongodb

import (
	"strconv"

	"github.com/databasifylabs/engine-sdk/dtos"
	"github.com/databasifylabs/engine-sdk/settings"
)

const (
	maxInt32         = 2147483647
	reservedMemoryMb = 1024
	minCacheGb       = 0.25
	minConnections   = 100
	maxConnections   = 20000
	keyCacheSize     = "cacheSizeGB"
	keyConnections   = "maxIncomingConnections"
	keySlowOperation = "slowOpThresholdMs"
	keyVerbosity     = "verbosity"
	keyTransactions  = "transactionLifetimeLimitSeconds"
	keyReadConcern   = "defaultReadConcern"
	keyWriteConcern  = "defaultWriteConcern"
)

func catalog() settings.Catalog {
	return settings.Catalog{Tune: tunedFor, Fields: settings.Fields(
		settings.Decimal(keyCacheSize, "Cache size", "Memory the WiredTiger cache may use, in gigabytes.", minCacheGb, 10000, "0.25").In("GB").TunedFromServer(),
		settings.Integer(keyConnections, "Max connections", "Most client connections accepted at once.", 1, 1000000, "1000000").Restarting().TunedFromServer(),
		settings.Choice(keyReadConcern, "Default read concern", "Default consistency of reads that name no read concern.", []string{"local", "available", "majority"}, "local"),
		settings.Choice(keyWriteConcern, "Default write concern", "Acknowledgement writes wait for when they name no write concern.", []string{"majority", "1"}, "majority"),
		settings.Integer(keyTransactions, "Transaction lifetime", "Seconds a multi-document transaction may stay open before it is aborted.", 1, maxInt32, "60").In("s"),
		settings.Integer(keySlowOperation, "Slow operation threshold", "Operations slower than this are logged and profiled.", 0, maxInt32, "100").In("ms"),
		settings.Integer(keyVerbosity, "Log verbosity", "Log message verbosity level.", 0, 5, "0"),
	)}
}

func tunedFor(resources dtos.Member) map[string]string {
	memory := max(resources.MemoryMb, 1)

	return map[string]string{
		keyCacheSize:   strconv.FormatFloat(max(minCacheGb, float64(memory-reservedMemoryMb)/2/1024), 'f', 2, 64),
		keyConnections: strconv.FormatInt(min(max(memory/2, minConnections), maxConnections), 10),
	}
}
