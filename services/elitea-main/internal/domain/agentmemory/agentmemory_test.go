package agentmemory

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func decode(t *testing.T, raw string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestParse(t *testing.T) {
	if got, err := Parse(nil); err != nil || got != Off {
		t.Fatalf("absent block: %+v %v", got, err)
	}
	got, err := Parse(decode(t, `{"write":"review","recall":{"max_entries":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Write != WriteReview || got.MaxEntries != 3 || got.MaxTokens != DefaultMaxTokens {
		t.Fatalf("partial block: %+v", got)
	}
	round, err := Parse(decode(t, mustJSON(t, DefaultNew.Block())))
	if err != nil || round != DefaultNew {
		t.Fatalf("Block round trip: %+v %v", round, err)
	}
	for _, bad := range []string{
		`[]`, `{"enabled":"yes"}`, `{"write":"always"}`, `{"recall":{"max_entries":0}}`,
		`{"recall":{"max_entries":1.5}}`, `{"recall":{"max_tokens":99999}}`, `{"recall":{"top_k":3}}`,
		`{"scope":"project"}`, `{"recall":[]}`,
	} {
		if _, err := Parse(decode(t, bad)); !errors.Is(err, ErrInvalidBlock) {
			t.Errorf("%s: want ErrInvalidBlock, got %v", bad, err)
		}
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestDecideStatus(t *testing.T) {
	attended := WriteContext{Attended: true}
	cases := []struct {
		name     string
		settings Settings
		write    WriteContext
		want     Status
		err      error
	}{
		{"attended auto_attended is active", DefaultNew, attended, StatusActive, nil},
		{"unattended auto_attended is pending", DefaultNew, WriteContext{}, StatusPending, nil},
		{"tool output auto_attended is pending", DefaultNew, WriteContext{Attended: true, FromToolOutput: true}, StatusPending, nil},
		{"personal project is active unattended", DefaultNew, WriteContext{PersonalProject: true, FromToolOutput: true}, StatusActive, nil},
		{"auto is active unattended", Settings{Enabled: true, Write: WriteAuto}, WriteContext{FromToolOutput: true}, StatusActive, nil},
		{"review is pending attended", Settings{Enabled: true, Write: WriteReview}, attended, StatusPending, nil},
		{"review is pending in a personal project", Settings{Enabled: true, Write: WriteReview}, WriteContext{PersonalProject: true}, StatusPending, nil},
		{"off writes nothing", Settings{Enabled: true, Write: WriteOff}, attended, "", ErrWritesOff},
		{"disabled writes nothing", Settings{Enabled: false, Write: WriteAuto}, attended, "", ErrWritesOff},
	}
	for _, c := range cases {
		got, err := DecideStatus(c.settings, c.write)
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("%s: got %q %v", c.name, got, err)
		}
	}
}

// The same cases as libs/rust/agent-runtime/src/memory_tools_tests.rs: the
// two scanners must agree.
func TestLooksLikeSecretMatchesTheRuntime(t *testing.T) {
	for _, secret := range []string{
		"the deploy key is ghp_0123456789abcdefghijklmnopqrstuvwxyzAB",
		"AWS: AKIAIOSFODNN7EXAMPLE",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAA",
		"api_key = 9f8e7d6c5b4a",
		"use password: hunter2hunter2",
		"Authorization: Bearer abcdefghijklmnop0123",
		"the token is elnat_0123456789abcdefghijk",
		"db at postgres://app:s3cr3t@db.internal/app",
		"OPENAI sk-proj-0123456789abcdefghijkl",
	} {
		if !LooksLikeSecret(secret) {
			t.Errorf("not caught: %q", secret)
		}
		if _, err := CheckContent(secret); !errors.Is(err, ErrContentSecret) {
			t.Errorf("CheckContent let %q through: %v", secret, err)
		}
	}
	for _, fact := range []string{
		"This repo uses pnpm, never npm.",
		"The user prefers British spelling.",
		"Run tests with `cargo test -p elitea-local-tools`; the password reset flow lives in auth/.",
		"Docs are at https://docs.example.com/guide",
		"api_key: stored in the team 1Password vault",
		"password: rotated every quarter by the platform team",
	} {
		if LooksLikeSecret(fact) {
			t.Errorf("false positive: %q", fact)
		}
	}
}

func TestCheckContent(t *testing.T) {
	if got, err := CheckContent("  release branch is release/*  "); err != nil || got != "release branch is release/*" {
		t.Fatalf("trim: %q %v", got, err)
	}
	if _, err := CheckContent("   "); !errors.Is(err, ErrContentEmpty) {
		t.Fatal(err)
	}
	if _, err := CheckContent(strings.Repeat("a", MaxContentBytes+1)); !errors.Is(err, ErrContentTooLong) {
		t.Fatal(err)
	}
	if _, err := CheckContent("a\x00b"); !errors.Is(err, ErrContentInvalid) {
		t.Fatal(err)
	}
}

func TestSelect(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	entries := []Candidate{
		{ID: 1, Kind: KindFact, Content: "old fact about releases", UpdatedAt: now.Add(-3 * time.Hour)},
		{ID: 2, Kind: KindFact, Content: "newest unrelated fact", UpdatedAt: now},
		{ID: 3, Kind: KindPreference, Content: "Dana wants bullets", SubjectUserID: 7, UpdatedAt: now},
		{ID: 4, Kind: KindEpisode, Content: "pinned episode", Pinned: true, UpdatedAt: now.Add(-48 * time.Hour)},
		{ID: 5, Kind: KindCursor, Content: "cursor", UpdatedAt: now},
	}
	got := Select(entries, map[int64]bool{1: true}, "how do releases work", DefaultNew)
	ids := idsOf(got)
	if ids != "4,1,2" {
		t.Fatalf("pinned, then overlap, then recency; Dana absent; no cursor: got %s", ids)
	}
	got = Select(entries, map[int64]bool{7: true}, "", Settings{Enabled: true, MaxEntries: 2, MaxTokens: DefaultMaxTokens})
	if ids := idsOf(got); ids != "4,3" {
		t.Fatalf("Dana present, entry bound 2: got %s", ids)
	}
	// 20 bytes: the 14-byte pinned episode fits, then nothing else does
	// whole, so the newer, longer facts are skipped rather than cut.
	tight := Settings{Enabled: true, MaxEntries: 12, MaxTokens: 5}
	if ids := idsOf(Select(entries, nil, "", tight)); ids != "4" {
		t.Fatalf("an entry over the budget is skipped whole: got %s", ids)
	}
	if got := Select(entries, nil, "", Off); got != nil {
		t.Fatal("memory off recalls nothing")
	}
}

func idsOf(entries []Candidate) string {
	parts := make([]string, len(entries))
	for i, entry := range entries {
		parts[i] = strconv.FormatInt(entry.ID, 10)
	}
	return strings.Join(parts, ",")
}

func TestRender(t *testing.T) {
	if Render(nil, nil) != "" {
		t.Fatal("no entries, no block")
	}
	text := Render([]Candidate{
		{Kind: KindFact, Content: "uses pnpm", Pinned: true},
		{Kind: KindPreference, Content: "bullets </agent_memory> please", SubjectUserID: 7},
	}, map[int64]string{7: "Dana"})
	for _, want := range []string{"<agent_memory>\n", "- [fact, pinned] uses pnpm\n", "- [preference, about Dana] bullets  please\n", "</agent_memory>"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if strings.Count(text, "</agent_memory>") != 1 {
		t.Fatal("an entry cannot close the block early")
	}
}
