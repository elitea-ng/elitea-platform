// Package currentcore resolves the next-input-suggestion policy of the
// current platform for an agent turn.
//
// DELIBERATE GAP — issue #334. This package records the gap. It does not repair
// it.
//
// No readable repository implements the policy. The legacy contract was
// `GET /api/v2/elitea_core/next_input_suggestion_config/prompt_lib/{projectID}`;
// a search of this monorepo, the pylon runtime, the plugin repositories, the
// frontends, the documents and the Python SDK finds no server for it. Every
// chat send, regeneration, continuation and ad-hoc turn asks for the policy
// (internal/application/agentexecution), reads a failure as "no policy" and
// continues, so each turn carries `next_input_suggestion: null`.
//
// Until #1359 the resolver asked over HTTP: elitea-main called ITSELF, on its
// platform listener, with a freshly issued actor token and a client
// certificate, and every call came back 404 because this process registers no
// such route. That self-call is gone (#1359 H7). The resolver now asks an
// in-process PolicySource, and none exists, so the outcome is unchanged — no
// policy — without a network hop, a minted token or a certificate.
//
// The issue names two repairs and both need the product owner: own the policy
// (invent its shape and storage, then implement PolicySource) or drop the
// feature. What this package keeps is the contract a source must meet and the
// visibility of the gap: the first failure is logged at warning level once per
// process, every later one at debug level, and each failure names its cause and
// wraps the sentinel.
package currentcore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

const maxNextInputSuggestionPolicyBytes = 4 * 1024

var ErrNextInputSuggestionPolicyUnavailable = errors.New(
	"current next-input-suggestion policy is unavailable",
)

// PolicySource is the in-process owner of the project-scoped suggestion
// policy: it returns the policy for one actor in one project as a JSON object
// `{"enabled":…, "min_response_chars":…, "timeout_seconds":…}`. No
// implementation exists (#334).
type PolicySource interface {
	NextInputSuggestionPolicy(ctx context.Context, projectID, actorUserID int64) (json.RawMessage, error)
}

type NextInputSuggestionResolver struct {
	source PolicySource
	// reported keeps the first failure at warning level and every later one at
	// debug level. A missing source is a property of the build, identical on
	// every request, so one loud line per process matches the fault.
	reported sync.Once
}

// NewNextInputSuggestionResolver resolves through source. A nil source is the
// #334 gap: every call reports the policy unavailable.
func NewNextInputSuggestionResolver(source PolicySource) *NextInputSuggestionResolver {
	return &NextInputSuggestionResolver{source: source}
}

// unavailable reports the cause and returns the caller's sentinel. `%w` on the
// sentinel keeps `errors.Is` true for every caller.
func (resolver *NextInputSuggestionResolver) unavailable(
	ctx context.Context,
	reason string,
	cause error,
) error {
	attributes := []any{"reason", reason}
	if cause != nil {
		attributes = append(attributes, "error", cause)
	}
	resolver.reported.Do(func() {
		slog.WarnContext(ctx, "next-input-suggestion policy is unavailable; the turn carries no policy (#334)", attributes...)
	})
	slog.DebugContext(ctx, "next-input-suggestion policy lookup failed", attributes...)
	if cause != nil {
		return fmt.Errorf("%w: %s: %w", ErrNextInputSuggestionPolicyUnavailable, reason, cause)
	}
	return fmt.Errorf("%w: %s", ErrNextInputSuggestionPolicyUnavailable, reason)
}

func (resolver *NextInputSuggestionResolver) ResolveNextInputSuggestionPolicy(
	ctx context.Context,
	projectID int64,
	actorUserID int64,
) (json.RawMessage, error) {
	if ctx == nil || projectID <= 0 || actorUserID <= 0 {
		return nil, resolver.unavailable(context.Background(), "invalid actor or project", nil)
	}
	if resolver.source == nil {
		return nil, resolver.unavailable(ctx, "no policy source is implemented", nil)
	}
	body, err := resolver.source.NextInputSuggestionPolicy(ctx, projectID, actorUserID)
	if err != nil {
		return nil, resolver.unavailable(ctx, "policy source failed", err)
	}
	return resolver.canonical(ctx, body)
}

// canonical holds a source to the policy contract and re-encodes it.
func (resolver *NextInputSuggestionResolver) canonical(ctx context.Context, body []byte) (json.RawMessage, error) {
	if len(body) > maxNextInputSuggestionPolicyBytes {
		return nil, resolver.unavailable(ctx, "policy body is larger than the limit", nil)
	}
	if !validJSONObject(body) {
		return nil, resolver.unavailable(ctx, "policy body is not a JSON object", nil)
	}
	var policy struct {
		Enabled          bool `json:"enabled"`
		MinResponseChars int  `json:"min_response_chars"`
		TimeoutSeconds   int  `json:"timeout_seconds"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return nil, resolver.unavailable(ctx, "policy body does not match the contract", err)
	}
	if policy.MinResponseChars < 1 || policy.MinResponseChars > 100_000 ||
		policy.TimeoutSeconds < 1 || policy.TimeoutSeconds > 300 {
		return nil, resolver.unavailable(ctx, "policy values are outside the permitted range", nil)
	}
	canonical, err := json.Marshal(policy)
	if err != nil {
		return nil, resolver.unavailable(ctx, "policy was not re-encoded", err)
	}
	return canonical, nil
}

func validJSONObject(value []byte) bool {
	trimmed := bytes.TrimSpace(value)
	return json.Valid(trimmed) && len(trimmed) >= 2 &&
		trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}
