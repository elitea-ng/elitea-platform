package main

import (
	"fmt"
	"time"
)

// shutdownTimeoutEnv bounds the drain after SIGTERM: in-flight requests and
// open SSE streams get this long to finish, then the public server closes
// what is still open (applicationServers.Shutdown) and the process exits.
// A closed execution stream loses nothing: the browser reconnects to
// another replica and resumes from its cursor in the durable replay.
//
// The pod's terminationGracePeriodSeconds must cover the preStop sleep, this
// drain and the exit work after it (pool close, the 5 s trace flush).
const shutdownTimeoutEnv = "ELITEA_SHUTDOWN_TIMEOUT"

// defaultShutdownTimeout is the drain the process had before it was a
// setting.
const defaultShutdownTimeout = 15 * time.Second

// maxShutdownTimeout keeps a typo from holding a terminating pod for hours.
const maxShutdownTimeout = 10 * time.Minute

// shutdownTimeoutFromEnv reads shutdownTimeoutEnv as a Go duration
// ("15s", "2m"). Unset or empty keeps the default.
func shutdownTimeoutFromEnv(lookup func(string) (string, bool)) (time.Duration, error) {
	raw, ok := lookup(shutdownTimeoutEnv)
	if !ok || raw == "" {
		return defaultShutdownTimeout, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed < time.Second || parsed > maxShutdownTimeout {
		return 0, fmt.Errorf("%s must be a duration between 1s and %s, got %q", shutdownTimeoutEnv, maxShutdownTimeout, raw)
	}
	return parsed, nil
}
