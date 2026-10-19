package main

import (
	"context"
	"fmt"
	"strconv"

	infradb "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

const databaseMaxConnectionsEnvironment = "ELITEA_DATABASE_MAX_CONNS"

func databasePoolConfig(
	dsn string,
	lookup func(string) (string, bool),
) (*pgxpool.Config, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database pool configuration: %w", err)
	}
	infradb.WithTCPKeepAlive(&config.ConnConfig.Config)
	raw, ok := lookup(databaseMaxConnectionsEnvironment)
	if !ok || raw == "" {
		return config, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || strconv.FormatInt(parsed, 10) != raw || parsed < 1 || parsed > 64 {
		return nil, fmt.Errorf("%s must be a canonical integer between 1 and 64", databaseMaxConnectionsEnvironment)
	}
	config.MaxConns = int32(parsed)
	return config, nil
}

func openDatabasePool(
	ctx context.Context,
	dsn string,
	lookup func(string) (string, bool),
) (*pgxpool.Pool, error) {
	config, err := databasePoolConfig(dsn, lookup)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("create database pool: %w", err)
	}
	return pool, nil
}
