package main

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// databaseMaxConnsEnv sizes the scheduler's PostgreSQL pool, under the name
// elitea-main reads for its own primary pool. Unset keeps the pgx default,
// max(4, numCPU), which depends on the node the pod lands on; the chart sets
// it, so the release's connection use is computable.
const databaseMaxConnsEnv = "ELITEA_DATABASE_MAX_CONNS"

var errDatabaseMaxConns = errors.New(databaseMaxConnsEnv + " must be a canonical integer between 1 and 64")

// databasePoolConfig parses dsn and applies databaseMaxConnsEnv.
func databasePoolConfig(dsn string, lookup func(string) (string, bool)) (*pgxpool.Config, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse the database pool configuration: %w", err)
	}
	raw, set := lookup(databaseMaxConnsEnv)
	if !set || raw == "" {
		return config, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || strconv.FormatInt(parsed, 10) != raw || parsed < 1 || parsed > 64 {
		return nil, errDatabaseMaxConns
	}
	config.MaxConns = int32(parsed)
	return config, nil
}
