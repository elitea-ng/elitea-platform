package main

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// databaseMaxConnsEnv sizes this replica's PostgreSQL pool. Unset keeps the
// pgx default, max(4, numCPU), which depends on the node the pod lands on;
// the chart sets it, so the release's connection use is replicas x this.
const databaseMaxConnsEnv = "GATEWAY_DATABASE_MAX_CONNS"

// errDatabaseMaxConns is an operator error in databaseMaxConnsEnv. main
// refuses to start on it: a DSN the pool cannot parse only disables the
// database features, and a typo in a sizing knob must not do that.
var errDatabaseMaxConns = errors.New(databaseMaxConnsEnv + " must be a canonical integer between 1 and 64")

// databasePoolConfig parses dsn and applies databaseMaxConnsEnv.
func databasePoolConfig(dsn string, lookup func(string) (string, bool)) (*pgxpool.Config, error) {
	raw, set := lookup(databaseMaxConnsEnv)
	var maxConns int64
	if set && raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || strconv.FormatInt(parsed, 10) != raw || parsed < 1 || parsed > 64 {
			return nil, errDatabaseMaxConns
		}
		maxConns = parsed
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse the database pool configuration: %w", err)
	}
	if maxConns > 0 {
		config.MaxConns = int32(maxConns)
	}
	return config, nil
}
