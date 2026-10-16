// Package config reads elitea-egress-proxy's configuration from the
// environment. Every variable has the prefix ELITEA_EGRESS_PROXY_. A value
// that does not parse is a startup error, never a silent default: a limit an
// operator set and the proxy ignored would read as enforced.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"
)

// Prefix is the prefix of every variable.
const Prefix = "ELITEA_EGRESS_PROXY_"

// Config is the proxy's configuration.
type Config struct {
	// ListenAddr is the proxy listener (HTTP CONNECT and absolute-form HTTP).
	ListenAddr string
	// AdminAddr serves /healthz, /readyz and /metrics. It is a separate port,
	// so the sandbox, which reaches only the proxy port, never sees it.
	AdminAddr string
	// PublicKeys is a file, or a directory of files, holding the PEM Ed25519
	// public keys that verify run tokens.
	PublicKeys string
	// KeysReloadInterval is how often the key set is read again, so a
	// rotation needs no restart. 0 turns the reload off.
	KeysReloadInterval time.Duration
	// DenyCIDRs are refused in addition to the shared forbidden and private
	// classes: the cluster's pod and service ranges, and any other range the
	// operator names.
	DenyCIDRs []*net.IPNet

	// MaxConnections bounds client connections to this replica.
	MaxConnections int
	// MaxHeaderBytes bounds one request's header block, and one upstream
	// response's header block on a forwarded request.
	MaxHeaderBytes int
	// ReadHeaderTimeout bounds the time a client takes to send its headers.
	ReadHeaderTimeout time.Duration
	// DialTimeout bounds one upstream connect.
	DialTimeout time.Duration

	// RunMaxConnections bounds one run's concurrent connections on this replica.
	RunMaxConnections int
	// RunMaxBytes bounds one run's bytes, up and down together, on this replica.
	RunMaxBytes int64
	// IdleTimeout closes a connection with no bytes in either direction for
	// this long.
	IdleTimeout time.Duration
	// MaxConnectionLifetime closes a connection this long after it opened. A
	// connection also closes when the run token expires.
	MaxConnectionLifetime time.Duration

	// SummaryInterval is how often a run's summary is logged while it is
	// active. The final summary is logged when its token expires.
	SummaryInterval time.Duration
	// DrainDelay is how long the proxy reports not-ready before it stops
	// accepting, so the Service removes the endpoint first.
	DrainDelay time.Duration
	// ShutdownTimeout bounds the drain of open connections.
	ShutdownTimeout time.Duration

	LogLevel slog.Level
}

// Defaults returns the configuration with every default set and no key path.
func Defaults() Config {
	return Config{
		ListenAddr:            ":3128",
		AdminAddr:             ":9090",
		KeysReloadInterval:    30 * time.Second,
		MaxConnections:        4096,
		MaxHeaderBytes:        32 << 10,
		ReadHeaderTimeout:     10 * time.Second,
		DialTimeout:           10 * time.Second,
		RunMaxConnections:     64,
		RunMaxBytes:           2 << 30,
		IdleTimeout:           5 * time.Minute,
		MaxConnectionLifetime: time.Hour,
		SummaryInterval:       time.Minute,
		DrainDelay:            5 * time.Second,
		ShutdownTimeout:       60 * time.Second,
		LogLevel:              slog.LevelInfo,
	}
}

// FromEnv reads the configuration through getenv (os.Getenv in production).
func FromEnv(getenv func(string) string) (Config, error) {
	c := Defaults()
	var errs []error
	str := func(name string, dst *string) {
		if v := strings.TrimSpace(getenv(Prefix + name)); v != "" {
			*dst = v
		}
	}
	dur := func(name string, dst *time.Duration, allowZero bool) {
		v := strings.TrimSpace(getenv(Prefix + name))
		if v == "" {
			return
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 || (d == 0 && !allowZero) {
			errs = append(errs, fmt.Errorf("%s%s: want a positive duration such as 30s, got %q", Prefix, name, v))
			return
		}
		*dst = d
	}
	integer := func(name string, dst *int) {
		v := strings.TrimSpace(getenv(Prefix + name))
		if v == "" {
			return
		}
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("%s%s: want a positive integer, got %q", Prefix, name, v))
			return
		}
		*dst = n
	}

	str("LISTEN_ADDR", &c.ListenAddr)
	str("ADMIN_ADDR", &c.AdminAddr)
	str("PUBLIC_KEYS", &c.PublicKeys)
	dur("KEYS_RELOAD_INTERVAL", &c.KeysReloadInterval, true)
	integer("MAX_CONNECTIONS", &c.MaxConnections)
	integer("MAX_HEADER_BYTES", &c.MaxHeaderBytes)
	dur("READ_HEADER_TIMEOUT", &c.ReadHeaderTimeout, false)
	dur("DIAL_TIMEOUT", &c.DialTimeout, false)
	integer("RUN_MAX_CONNECTIONS", &c.RunMaxConnections)
	if v := strings.TrimSpace(getenv(Prefix + "RUN_MAX_BYTES")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("%sRUN_MAX_BYTES: want a positive integer of bytes, got %q", Prefix, v))
		} else {
			c.RunMaxBytes = n
		}
	}
	dur("IDLE_TIMEOUT", &c.IdleTimeout, false)
	dur("MAX_CONNECTION_LIFETIME", &c.MaxConnectionLifetime, false)
	dur("SUMMARY_INTERVAL", &c.SummaryInterval, false)
	dur("DRAIN_DELAY", &c.DrainDelay, true)
	dur("SHUTDOWN_TIMEOUT", &c.ShutdownTimeout, false)

	if v := strings.TrimSpace(getenv(Prefix + "DENY_CIDRS")); v != "" {
		for _, raw := range strings.Split(v, ",") {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			_, block, err := net.ParseCIDR(raw)
			if err != nil {
				errs = append(errs, fmt.Errorf("%sDENY_CIDRS: %q is not a CIDR block", Prefix, raw))
				continue
			}
			c.DenyCIDRs = append(c.DenyCIDRs, block)
		}
	}
	if v := strings.TrimSpace(getenv(Prefix + "LOG_LEVEL")); v != "" {
		if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
			errs = append(errs, fmt.Errorf("%sLOG_LEVEL: want debug, info, warn or error, got %q", Prefix, v))
		}
	}

	if c.PublicKeys == "" {
		errs = append(errs, fmt.Errorf("%sPUBLIC_KEYS is required: the file or directory of run-token public keys", Prefix))
	}
	if c.MaxHeaderBytes < 16<<10 {
		errs = append(errs, fmt.Errorf("%sMAX_HEADER_BYTES must be at least 16384: a run token of up to 8 KiB travels base64-encoded in a header", Prefix))
	}
	return c, errors.Join(errs...)
}
