package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	DSN      string
	MaxConns int32
	MinConns int32
}

type Pool struct {
	*pgxpool.Pool
}

func NewPool(ctx context.Context, cfg Config) (*Pool, error) {
	poolCfg, err := poolConfig(cfg)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return &Pool{Pool: pool}, nil
}

func poolConfig(cfg Config) (*pgxpool.Config, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("db: parse config: %w", err)
	}
	WithTCPKeepAlive(&poolCfg.ConnConfig.Config)
	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	return poolCfg, nil
}

func (p *Pool) Ping(ctx context.Context) error {
	return p.Pool.Ping(ctx)
}
