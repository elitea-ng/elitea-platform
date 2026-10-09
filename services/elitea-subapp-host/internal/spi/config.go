// Package spi is the sub-application host: the provider SPI, served once.
//
// It is the Go port of the shell every provider service on this platform
// has to carry — routes, the invocation registry, the error contract, the
// mutual-TLS and identity gates, slots, strict settings — which until
// ADR-0023 existed only inside services/elitea-deepwiki, in Python, because
// that provider's engine is Python. Nothing generic in the shell needs
// Python, and a second provider would have carried a second copy.
//
// The contract is frozen (conformance/provider/spi/contract.json, recorded
// from the legacy plugin): the paths, the identity headers, the status
// vocabulary, the ten error categories and the read-once custom_events
// envelope do not change here. Where the Python shell's behaviour is a quirk
// the fixtures pin — the classifier's precedence, an unconditionally async
// invoke — this port reproduces it and the test says so.
package spi

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ErrConfig is the class of every settings refusal.
var ErrConfig = errors.New("invalid host configuration")

// Settings is what the host reads from its environment.
//
// The prefix is the sub-application's (ELITEA_DEEPWIKI_, ELITEA_INVENTORY_,
// …): one host binary per application, one namespace per application. The
// legacy DEEPWIKI_* aliases the Python shell honoured are honoured here too,
// under the same names, so a deployment written for it keeps working.
type Settings struct {
	Prefix string

	ServiceLocationURL         string
	ScratchPath                string
	JobsEnabled                bool
	MaxConcurrentJobs          int
	MaxParallelWorkers         int
	Namespace                  string
	InvocationRetentionSeconds int

	TLSCertFile string
	TLSKeyFile  string
	TLSCAFile   string
	// CallbackCAFile is the bundle trusted on the CALLBACK hop — the
	// artifact uploads to `llm_settings.api_base` — when that hop is TLS
	// through platform-edge (ADR-0027), whose certificate the runtime CA
	// signs. It is separate from TLSCAFile, which is the client CA of this
	// process's own mTLS listener. Empty keeps the old behaviour: the
	// callback trusts TLSCAFile (see CallbackCA).
	CallbackCAFile string
	IdentitySecret string
	GitAllowlist   string
	ListenAddr     string
	// DatabaseURL selects the durable invocation store (ADR-0023 H2b); empty
	// keeps invocations in memory, and /health says so.
	DatabaseURL string
	// EngineSocket is the Unix socket a sidecar engine listens on, for an
	// application whose runner reaches its engine out of process (ADR-0023
	// H2). Empty when the application has no such engine.
	EngineSocket string
}

// legacyAliases are the pre-prefix names the Python shell also read.
var legacyAliases = map[string]string{
	"SLOTS_MODE":           "DEEPWIKI_JOBS_ENABLED",
	"MAX_CONCURRENT_JOBS":  "DEEPWIKI_MAX_CONCURRENT_JOBS",
	"MAX_PARALLEL_WORKERS": "DEEPWIKI_MAX_PARALLEL_WORKERS",
	"NAMESPACE":            "DEEPWIKI_NAMESPACE",
}

// Lookup is the environment: os.LookupEnv, or a test's map.
type Lookup func(string) (string, bool)

// SettingsFromEnv reads the settings under prefix. Every refusal names the
// variable and what it must be; a variable with an unknown value never
// silently falls back to the default.
func SettingsFromEnv(prefix string, lookup Lookup) (Settings, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if prefix == "" {
		return Settings{}, fmt.Errorf("%w: an environment prefix is required", ErrConfig)
	}
	raw := func(name string, fallback string) string {
		if value, ok := lookup(prefix + name); ok {
			return value
		}
		if alias, ok := legacyAliases[name]; ok {
			if value, ok := lookup(alias); ok {
				return value
			}
		}
		return fallback
	}
	integer := func(name string, fallback, minimum int) (int, error) {
		value := strings.TrimSpace(raw(name, ""))
		if value == "" {
			return fallback, nil
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("%w: %s%s must be an integer, got %q", ErrConfig, prefix, name, value)
		}
		if parsed < minimum {
			return 0, fmt.Errorf("%w: %s%s must be >= %d, got %d", ErrConfig, prefix, name, minimum, parsed)
		}
		return parsed, nil
	}
	boolean := func(name string, fallback bool) (bool, error) {
		value := strings.ToLower(strings.TrimSpace(raw(name, "")))
		switch value {
		case "":
			return fallback, nil
		case "1", "true", "yes", "on":
			return true, nil
		case "0", "false", "no", "off":
			return false, nil
		}
		return false, fmt.Errorf("%w: %s%s must be a boolean, got %q", ErrConfig, prefix, name, value)
	}

	s := Settings{
		Prefix:             prefix,
		ServiceLocationURL: raw("SERVICE_LOCATION_URL", "http://127.0.0.1:8080"),
		ScratchPath:        raw("SCRATCH_PATH", "/tmp/"+strings.ToLower(strings.Trim(strings.TrimPrefix(prefix, "ELITEA_"), "_"))),
		Namespace:          raw("NAMESPACE", "deepwiki"),
		TLSCertFile:        raw("TLS_CERTFILE", ""),
		TLSKeyFile:         raw("TLS_KEYFILE", ""),
		TLSCAFile:          raw("TLS_CA_FILE", ""),
		CallbackCAFile:     raw("CALLBACK_CA_FILE", ""),
		IdentitySecret:     raw("IDENTITY_SECRET", ""),
		GitAllowlist:       raw("GIT_ALLOWLIST", ""),
		ListenAddr:         raw("LISTEN_ADDR", ":8080"),
		EngineSocket:       raw("ENGINE_SOCKET", ""),
		DatabaseURL:        raw("DATABASE_URL", ""),
	}
	var err error
	if s.JobsEnabled, err = boolean("SLOTS_MODE", false); err != nil {
		return Settings{}, err
	}
	if s.MaxConcurrentJobs, err = integer("MAX_CONCURRENT_JOBS", 3, 1); err != nil {
		return Settings{}, err
	}
	if s.MaxParallelWorkers, err = integer("MAX_PARALLEL_WORKERS", 1, 1); err != nil {
		return Settings{}, err
	}
	if s.InvocationRetentionSeconds, err = integer("INVOCATION_RETENTION_SECONDS", 3600, 1); err != nil {
		return Settings{}, err
	}
	if (s.TLSCertFile == "") != (s.TLSKeyFile == "") {
		return Settings{}, fmt.Errorf("%w: %sTLS_CERTFILE and %sTLS_KEYFILE must be set together", ErrConfig, prefix, prefix)
	}
	if s.TLSCAFile != "" && s.TLSCertFile == "" {
		return Settings{}, fmt.Errorf("%w: %sTLS_CA_FILE needs a server certificate to require client ones", ErrConfig, prefix)
	}
	return s, nil
}

// TerminatesMTLS reports whether this process is the mutual-TLS terminus: a
// server certificate and a client CA, which is the condition under which the
// listener requires and verifies a client certificate at the handshake.
func (s Settings) TerminatesMTLS() bool {
	return s.TLSCertFile != "" && s.TLSKeyFile != "" && s.TLSCAFile != ""
}

// MTLSRequired reports whether the SPI routes refuse a hop that is not
// mutually authenticated — on whenever a client CA is configured.
func (s Settings) MTLSRequired() bool { return s.TLSCAFile != "" }

// CallbackCA is the bundle the callback hop trusts: CALLBACK_CA_FILE when
// set, else TLS_CA_FILE, which is what the hop trusted before the two were
// separated (the engine reads the same pair, in the same order).
func (s Settings) CallbackCA() string {
	if s.CallbackCAFile != "" {
		return s.CallbackCAFile
	}
	return s.TLSCAFile
}
