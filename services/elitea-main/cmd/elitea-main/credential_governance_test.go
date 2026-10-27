package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialgovernance/profile"
)

type memoryRevisions struct {
	stored []profile.PolicyRevision
}

func (m *memoryRevisions) Revision(_ context.Context, id string) (profile.PolicyRevision, error) {
	for _, revision := range m.stored {
		if revision.ID == id {
			return revision, nil
		}
	}
	return profile.PolicyRevision{}, profile.ErrRevisionNotFound
}

func (m *memoryRevisions) PutRevision(_ context.Context, revision profile.PolicyRevision) error {
	m.stored = append(m.stored, revision)
	return nil
}

type refusingPrerequisite struct{}

func (refusingPrerequisite) Name() string { return "vault" }
func (refusingPrerequisite) Check(context.Context, profile.PolicyRevision) error {
	return errors.New("not configured")
}

const cliPolicy = `{"schema":"elitea.credential-policy.v1","allowed_identity_modes":["dedicated_agent"],"vault_required_credential_classes":[],"max_mandate_ttl_seconds":3600,"audit_backlog_alert_threshold":100}`

func TestCredentialPolicyPut(t *testing.T) {
	ctx := context.Background()
	read := func(body string) func(string) ([]byte, error) {
		return func(string) ([]byte, error) { return []byte(body), nil }
	}
	args := []string{"--id", "2026-10-r1", "--file", "policy.json", "--operator", "ops"}

	store := &memoryRevisions{}
	var out, errOut bytes.Buffer
	if code := credentialPolicyPut(ctx, args, store, read(cliPolicy), nil, &out, &errOut); code != 0 {
		t.Fatalf("put: code %d: %s", code, errOut.String())
	}
	if len(store.stored) != 1 || store.stored[0].CreatedBy != "ops" || store.stored[0].VerifyDigest() != nil {
		t.Fatalf("stored = %+v", store.stored)
	}
	if !strings.Contains(out.String(), profile.EnvPolicyRevision+"=2026-10-r1") {
		t.Fatalf("output does not say how to activate: %s", out.String())
	}

	for name, tc := range map[string]struct {
		body   string
		args   []string
		prereq func(context.Context) ([]profile.Prerequisite, error)
	}{
		"no identity mode": {body: strings.Replace(cliPolicy, `["dedicated_agent"]`, `[]`, 1), args: args},
		"unknown field":    {body: strings.Replace(cliPolicy, `"schema"`, `"x":1,"schema"`, 1), args: args},
		"bad id":           {body: cliPolicy, args: []string{"--id", "Bad", "--file", "f", "--operator", "ops"}},
		"no operator":      {body: cliPolicy, args: []string{"--id", "r2", "--file", "f"}},
		"prerequisite": {body: cliPolicy, args: args, prereq: func(context.Context) ([]profile.Prerequisite, error) {
			return []profile.Prerequisite{refusingPrerequisite{}}, nil
		}},
	} {
		t.Run(name, func(t *testing.T) {
			refused := &memoryRevisions{}
			if code := credentialPolicyPut(ctx, tc.args, refused, read(tc.body), tc.prereq, &bytes.Buffer{}, &bytes.Buffer{}); code == 0 || len(refused.stored) != 0 {
				t.Fatalf("code %d stored %d: must refuse", code, len(refused.stored))
			}
		})
	}
}

func TestCredentialGovernanceAtBootRefusesEnabledWithoutRevision(t *testing.T) {
	lookup := func(key string) (string, bool) {
		if key == profile.EnvEnabled {
			return "true", true
		}
		return "", false
	}
	if _, _, err := credentialGovernanceAtBoot(context.Background(), lookup, nil, nil); !errors.Is(err, profile.ErrInvalidConfig) {
		t.Fatalf("err = %v, want ErrInvalidConfig", err)
	}
}
