package currentcore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type policySourceStub struct {
	body               []byte
	err                error
	projectID, actorID int64
}

func (stub *policySourceStub) NextInputSuggestionPolicy(_ context.Context, projectID, actorUserID int64) (json.RawMessage, error) {
	stub.projectID, stub.actorID = projectID, actorUserID
	return stub.body, stub.err
}

// The #334 gap, in process: with no source every turn gets the sentinel and a
// reason, and nothing is called over the network (there is no client to call
// with).
func TestNextInputSuggestionResolverWithoutASourceReportsTheGap(t *testing.T) {
	resolver := NewNextInputSuggestionResolver(nil)
	_, err := resolver.ResolveNextInputSuggestionPolicy(context.Background(), 7, 11)
	if !errors.Is(err, ErrNextInputSuggestionPolicyUnavailable) || !strings.Contains(err.Error(), "no policy source") {
		t.Fatalf("error=%v, want the unavailable sentinel naming the missing source", err)
	}
}

func TestNextInputSuggestionResolverUsesExactActorAndProject(t *testing.T) {
	source := &policySourceStub{body: []byte(` {"timeout_seconds":15,"enabled":true,"min_response_chars":150} `)}
	policy, err := NewNextInputSuggestionResolver(source).ResolveNextInputSuggestionPolicy(context.Background(), 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	if source.projectID != 7 || source.actorID != 11 {
		t.Fatalf("source asked for project %d actor %d", source.projectID, source.actorID)
	}
	if string(policy) != `{"enabled":true,"min_response_chars":150,"timeout_seconds":15}` {
		t.Fatalf("policy=%s, want the canonical encoding", policy)
	}
}

func TestNextInputSuggestionResolverRefusesInvalidInput(t *testing.T) {
	source := &policySourceStub{body: []byte(`{"enabled":true,"min_response_chars":150,"timeout_seconds":15}`)}
	resolver := NewNextInputSuggestionResolver(source)
	for name, call := range map[string]func() error{
		"nil context": func() error {
			//nolint:staticcheck // a nil context is the case under test
			_, err := resolver.ResolveNextInputSuggestionPolicy(nil, 7, 11)
			return err
		},
		"no project": func() error {
			_, err := resolver.ResolveNextInputSuggestionPolicy(context.Background(), 0, 11)
			return err
		},
		"no actor": func() error {
			_, err := resolver.ResolveNextInputSuggestionPolicy(context.Background(), 7, 0)
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrNextInputSuggestionPolicyUnavailable) {
			t.Errorf("%s: error=%v, want the unavailable sentinel", name, err)
		}
	}
}

// Each failure names its cause and keeps the sentinel comparable (#334).
func TestNextInputSuggestionResolverNamesTheCause(t *testing.T) {
	sourceFailure := errors.New("policy store is down")
	for name, test := range map[string]struct {
		source *policySourceStub
		want   string
	}{
		"source error": {source: &policySourceStub{err: sourceFailure}, want: "policy source failed"},
		"unknown field": {
			source: &policySourceStub{body: []byte(`{"enabled":true,"min_response_chars":150,"timeout_seconds":15,"extra":true}`)},
			want:   "does not match the contract",
		},
		"invalid bounds": {
			source: &policySourceStub{body: []byte(`{"enabled":true,"min_response_chars":0,"timeout_seconds":15}`)},
			want:   "outside the permitted range",
		},
		"not an object": {source: &policySourceStub{body: []byte(`[]`)}, want: "not a JSON object"},
		"too large": {
			source: &policySourceStub{body: []byte(`{"enabled":true,"pad":"` + strings.Repeat("x", maxNextInputSuggestionPolicyBytes) + `"}`)},
			want:   "larger than the limit",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewNextInputSuggestionResolver(test.source).ResolveNextInputSuggestionPolicy(context.Background(), 7, 11)
			if !errors.Is(err, ErrNextInputSuggestionPolicyUnavailable) {
				t.Fatalf("error=%v, want the unavailable sentinel", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%q, want it to name %q", err, test.want)
			}
			if test.source.err != nil && !errors.Is(err, test.source.err) {
				t.Fatalf("error=%v lost the source's cause", err)
			}
		})
	}
}
