package executions

import (
	"errors"
	"fmt"
	"strconv"
)

const (
	EnvSSEMaxStreams          = "ELITEA_RUNTIME_SSE_MAX_STREAMS"
	EnvSSEMaxStreamsPrincipal = "ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PRINCIPAL"
	EnvSSEMaxStreamsProject   = "ELITEA_RUNTIME_SSE_MAX_STREAMS_PER_PROJECT"
)

// SSEStreamLimits bounds the long-lived execution-event streams one main
// replica serves. The bounds are process-local by design: the durable
// repository stays authoritative, and the limits only bound how many open
// streams this process holds.
type SSEStreamLimits struct {
	MaxStreams      int
	MaxPerPrincipal int
	MaxPerProject   int
}

// DefaultSSEStreamLimits returns the built-in process-local profile.
func DefaultSSEStreamLimits() SSEStreamLimits {
	return SSEStreamLimits{
		MaxStreams:      defaultMaxActiveSSEStreams,
		MaxPerPrincipal: defaultMaxActiveSSEStreamsPerPrincipal,
		MaxPerProject:   defaultMaxActiveSSEStreamsPerProject,
	}
}

// SSEStreamLimitsFromEnv reads the stream limits from the environment. An
// unset or empty variable keeps the default for that limit. A per-principal
// or per-project limit above the global limit is an error.
func SSEStreamLimitsFromEnv(lookup func(string) (string, bool)) (SSEStreamLimits, error) {
	if lookup == nil {
		return SSEStreamLimits{}, errors.New("SSE stream limits require an environment lookup")
	}
	limits := DefaultSSEStreamLimits()
	var err error
	if limits.MaxStreams, err = optionalSSEStreamLimit(lookup, EnvSSEMaxStreams, limits.MaxStreams); err != nil {
		return SSEStreamLimits{}, err
	}
	if limits.MaxPerPrincipal, err = optionalSSEStreamLimit(lookup, EnvSSEMaxStreamsPrincipal, limits.MaxPerPrincipal); err != nil {
		return SSEStreamLimits{}, err
	}
	if limits.MaxPerProject, err = optionalSSEStreamLimit(lookup, EnvSSEMaxStreamsProject, limits.MaxPerProject); err != nil {
		return SSEStreamLimits{}, err
	}
	if limits.MaxPerPrincipal > limits.MaxStreams {
		return SSEStreamLimits{}, fmt.Errorf(
			"%s must not exceed %s",
			EnvSSEMaxStreamsPrincipal,
			EnvSSEMaxStreams,
		)
	}
	if limits.MaxPerProject > limits.MaxStreams {
		return SSEStreamLimits{}, fmt.Errorf(
			"%s must not exceed %s",
			EnvSSEMaxStreamsProject,
			EnvSSEMaxStreams,
		)
	}
	return limits, nil
}

func optionalSSEStreamLimit(lookup func(string) (string, bool), name string, fallback int) (int, error) {
	raw, _ := lookup(name)
	if raw == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || parsed <= 0 || strconv.FormatInt(parsed, 10) != raw {
		return 0, fmt.Errorf("%s must be a canonical positive integer", name)
	}
	return int(parsed), nil
}
