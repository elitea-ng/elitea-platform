//go:build conformance

package governed_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/conformance/governed/canary"
)

// The phases deploy/scripts/governed-conformance.sh drives the stack
// through. Each top-level test belongs to exactly one, and refuses to run in
// another: a case that ran against the wrong stack state would prove
// nothing, and must not read as a pass.
const (
	phaseOff       = "off"       // governance off, the stack as installed
	phaseOn        = "on"        // governance on, governed worker
	phaseOldWorker = "oldworker" // governance on, worker without the governed context
	phaseRestored  = "restored"  // governance on, governed worker back
	phaseRollback  = "rollback"  // governance off again, governed worker
)

// config is everything the suite reads from its environment. The script sets
// all of it; the defaults match the script and the E2E seed.
type config struct {
	// Origin is the published elitea-main origin (http://localhost:8094).
	Origin string
	Phase  string
	// Stack is the script itself: `<Stack> compose <args>` runs any compose
	// verb against the stack's project and overlays.
	Stack string
	// MocksAdmin is the governed mocks' admin listener (journal, token mint).
	MocksAdmin string
	// PATKey is the deployment's PAT signing key (raw bytes, HS512).
	PATKey string
	// Project is the governed fixture project (seed-governed).
	Project int64
	// Model is the chat model seed-llm wrote (the offline mock).
	Model string
	// Evidence is where canaries.json and state.json go, and where the
	// script collects the dumps the canary scan reads.
	Evidence string
	// The personas. Admin and Approver are both global admins: every
	// two-person transition is proposed by one and approved by the other,
	// and neither id is ever sent in a body.
	User, Admin, Approver, Member string
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

var (
	loadOnce sync.Once
	loaded   config
	loadErr  error
)

func loadConfig(t *testing.T, phase string) config {
	t.Helper()
	loadOnce.Do(func() {
		project, err := strconv.ParseInt(env("ELITEA_GOVERNED_PROJECT_ID", "90700"), 10, 64)
		if err != nil {
			loadErr = fmt.Errorf("ELITEA_GOVERNED_PROJECT_ID: %w", err)
			return
		}
		loaded = config{
			Origin:     strings.TrimRight(env("ELITEA_GOVERNED_ORIGIN", ""), "/"),
			Phase:      env("ELITEA_GOVERNED_PHASE", ""),
			Stack:      env("ELITEA_GOVERNED_STACK", ""),
			MocksAdmin: strings.TrimRight(env("ELITEA_GOVERNED_MOCKS_ADMIN", "http://127.0.0.1:18480"), "/"),
			PATKey:     env("ELITEA_GOVERNED_PAT_SIGNING_KEY", ""),
			Project:    project,
			Model:      env("ELITEA_GOVERNED_MODEL", "vllm/E2E-MOCK-MODEL"),
			Evidence:   env("ELITEA_GOVERNED_EVIDENCE_DIR", ""),
			User:       env("ELITEA_GOVERNED_USER", "e2e-chat@autotest.local"),
			Admin:      env("ELITEA_GOVERNED_ADMIN", "e2e-admin@autotest.local"),
			Approver:   env("ELITEA_GOVERNED_APPROVER", "e2e-gov-approver@autotest.local"),
			Member:     env("ELITEA_GOVERNED_MEMBER", "e2e-gov-member@autotest.local"),
		}
		// A missing setting is a FAILURE, not a skip: this file compiles
		// only under the `conformance` tag, so whoever built it meant to run
		// it against a stack.
		for name, value := range map[string]string{
			"ELITEA_GOVERNED_ORIGIN": loaded.Origin, "ELITEA_GOVERNED_PHASE": loaded.Phase,
			"ELITEA_GOVERNED_STACK": loaded.Stack, "ELITEA_GOVERNED_PAT_SIGNING_KEY": loaded.PATKey,
			"ELITEA_GOVERNED_EVIDENCE_DIR": loaded.Evidence,
		} {
			if value == "" {
				loadErr = fmt.Errorf("%s is not set; run deploy/scripts/governed-conformance.sh", name)
				return
			}
		}
	})
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if loaded.Phase != phase {
		t.Fatalf("%s belongs to phase %q and the stack is in phase %q: run it through deploy/scripts/governed-conformance.sh",
			t.Name(), phase, loaded.Phase)
	}
	return loaded
}

func testContext(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func shortID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func sha(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// ── the stack: compose, psql, the CLI ────────────────────────────────────────

// stack runs `<script> compose <args>` and returns stdout. stdin may be nil.
func (cfg config) stack(ctx context.Context, stdin string, args ...string) (string, string, error) {
	command := exec.CommandContext(ctx, cfg.Stack, append([]string{"compose"}, args...)...)
	if stdin != "" {
		command.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

// psql runs sql against the platform database and returns the unaligned,
// tuples-only rows. vars become psql variables, so a value reaches the SQL
// as :'name' and is never spliced into its text. It only READS in this
// suite: governance rows are written through the public API, never here.
func (cfg config) psql(ctx context.Context, t *testing.T, sql string, vars map[string]string) []string {
	t.Helper()
	args := []string{"exec", "-T", "postgres", "psql", "-X", "-q", "-U", "elitea", "-d", "elitea",
		"-tA", "-v", "ON_ERROR_STOP=1"}
	for name, value := range vars {
		args = append(args, "-v", name+"="+value)
	}
	out, stderr, err := cfg.stack(ctx, sql, args...)
	if err != nil {
		t.Fatalf("psql: %v\n%s", err, stderr)
	}
	var rows []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			rows = append(rows, line)
		}
	}
	return rows
}

func (cfg config) psqlOne(ctx context.Context, t *testing.T, sql string, vars map[string]string) string {
	t.Helper()
	rows := cfg.psql(ctx, t, sql, vars)
	if len(rows) != 1 {
		t.Fatalf("want exactly one row, got %d: %q", len(rows), rows)
	}
	return rows[0]
}

func (cfg config) psqlInt(ctx context.Context, t *testing.T, sql string, vars map[string]string) int64 {
	t.Helper()
	value, err := strconv.ParseInt(cfg.psqlOne(ctx, t, sql, vars), 10, 64)
	if err != nil {
		t.Fatalf("want an integer: %v", err)
	}
	return value
}

// mainCLI runs the elitea-main binary's operator CLI inside its container.
func (cfg config) mainCLI(ctx context.Context, args ...string) (string, string, error) {
	return cfg.stack(ctx, "", append([]string{"exec", "-T", "elitea-main", "/elitea-main"}, args...)...)
}

// ── personas and the API client ──────────────────────────────────────────────

type persona struct {
	Email  string
	UserID int64
	token  string
	origin string
	http   *http.Client
}

func (cfg config) persona(ctx context.Context, t *testing.T, email string) *persona {
	t.Helper()
	row := cfg.psqlOne(ctx, t, `
SELECT u.id || ' ' || t.uuid
  FROM public.auth_core__user u
  JOIN public.auth_core__token t ON t.user_id = u.id AND t.uuid IS NOT NULL
   AND (t.expires IS NULL OR t.expires > (clock_timestamp() AT TIME ZONE 'UTC'))
 WHERE u.email = :'email'
 ORDER BY t.id
 LIMIT 1;`, map[string]string{"email": email})
	idText, uuid, ok := strings.Cut(row, " ")
	id, err := strconv.ParseInt(idText, 10, 64)
	if !ok || err != nil || uuid == "" {
		t.Fatalf("persona %s: no user with an active PAT (row %q); run the script's seed-governed step", email, row)
	}
	key, err := os.ReadFile(cfg.PATKey)
	if err != nil {
		t.Fatalf("read the PAT signing key: %v", err)
	}
	return &persona{Email: email, UserID: id, token: mintPAT(key, uuid), origin: cfg.Origin,
		http: &http.Client{Timeout: 60 * time.Second}}
}

// mintPAT signs the deployment's PAT form: HS512 over {"uuid","expires":null}.
func mintPAT(key []byte, uuid string) string {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"HS512","typ":"JWT"}`))
	payload := enc.EncodeToString([]byte(`{"uuid":"` + uuid + `","expires":null}`))
	mac := hmac.New(sha512.New, key)
	mac.Write([]byte(header + "." + payload))
	return header + "." + payload + "." + enc.EncodeToString(mac.Sum(nil))
}

// response is one API answer.
type response struct {
	Method, Path string
	Status       int
	Header       http.Header
	Body         []byte
}

func (r response) String() string {
	body := string(r.Body)
	if len(body) > 600 {
		body = body[:600] + "…"
	}
	return fmt.Sprintf("%s %s -> %d %s", r.Method, r.Path, r.Status, body)
}

func (r response) JSON(t *testing.T, into any) {
	t.Helper()
	if err := json.Unmarshal(r.Body, into); err != nil {
		t.Fatalf("%s: not JSON: %v", r, err)
	}
}

func (r response) Object(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	r.JSON(t, &out)
	return out
}

// raw sends one request and returns whatever came back.
func (p *persona) raw(ctx context.Context, t *testing.T, method, path string, body any, header http.Header) response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	url := path
	if !strings.HasPrefix(path, "http") {
		url = p.origin + path
	}
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, values := range header {
		request.Header[name] = values
	}
	answer, err := p.http.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = answer.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(answer.Body, 8<<20))
	return response{Method: method, Path: path, Status: answer.StatusCode, Header: answer.Header, Body: raw}
}

// do sends one request and FAILS on any status not in want. There is no
// "not found means not supported" path anywhere in this suite: a route that
// answers 404 is a failure, never a skip (absence is not correctness).
func (p *persona) do(ctx context.Context, t *testing.T, method, path string, body any, want ...int) response {
	t.Helper()
	answer := p.raw(ctx, t, method, path, body, nil)
	if !slices.Contains(want, answer.Status) {
		t.Fatalf("%s (as %s): want status %v", answer, p.Email, want)
	}
	return answer
}

// ── the mocks ────────────────────────────────────────────────────────────────

type journalEntry struct {
	Seq              int64  `json:"seq"`
	Host             string `json:"host"`
	Method           string `json:"method"`
	Path             string `json:"path"`
	Scheme           string `json:"scheme"`
	CredentialSHA256 string `json:"credential_sha256"`
	Status           int    `json:"status"`
}

func (cfg config) journal(ctx context.Context, t *testing.T, host string) []journalEntry {
	t.Helper()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.MocksAdmin+"/__journal?host="+host, nil)
	answer, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("read the mocks' journal: %v", err)
	}
	defer func() { _ = answer.Body.Close() }()
	var body struct {
		Entries []journalEntry `json:"entries"`
	}
	if answer.StatusCode != http.StatusOK || json.NewDecoder(answer.Body).Decode(&body) != nil {
		t.Fatalf("the mocks' journal answered %d", answer.StatusCode)
	}
	return body.Entries
}

// callsWith are the journalled calls that presented credential value.
func (cfg config) callsWith(ctx context.Context, t *testing.T, host, value string) []journalEntry {
	t.Helper()
	digest := sha(value)
	var out []journalEntry
	for _, e := range cfg.journal(ctx, t, host) {
		if e.CredentialSHA256 == digest {
			out = append(out, e)
		}
	}
	return out
}

func (cfg config) mintInboundMCPToken(ctx context.Context, t *testing.T, body map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(body)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, cfg.MocksAdmin+"/__mint/mcp-inbound", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	answer, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("mint an inbound MCP token: %v", err)
	}
	defer func() { _ = answer.Body.Close() }()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if answer.StatusCode != http.StatusOK || json.NewDecoder(answer.Body).Decode(&out) != nil || out.AccessToken == "" {
		t.Fatalf("the mocks minted no inbound MCP token (%d)", answer.StatusCode)
	}
	return out.AccessToken
}

// ── evidence: canaries and cross-phase state ─────────────────────────────────

var evidenceMu sync.Mutex

// newCanary mints a canary and records it in canaries.json BEFORE it is
// used, so a run that dies half-way still has every seeded value scanned.
func (cfg config) newCanary(t *testing.T, label string) string {
	t.Helper()
	c, err := canary.New(label + "-" + shortID())
	if err != nil {
		t.Fatal(err)
	}
	evidenceMu.Lock()
	defer evidenceMu.Unlock()
	path := filepath.Join(cfg.Evidence, "canaries.json")
	var list []canary.Canary
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("canaries.json is corrupt: %v", err)
		}
	}
	list = append(list, c)
	raw, _ := json.MarshalIndent(list, "", "  ")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return c.Value
}

// state is what one phase leaves for a later one.
type state struct {
	Legacy    *turnFixture `json:"legacy,omitempty"`
	Paused    *pausedRun   `json:"paused,omitempty"`
	OldWorker *turnFixture `json:"old_worker,omitempty"`
	Held      *heldRun     `json:"held,omitempty"`
}

// turnFixture is an agent a later phase can chat with.
type turnFixture struct {
	ApplicationID    int64  `json:"application_id"`
	VersionID        int64  `json:"version_id"`
	ToolkitID        int64  `json:"toolkit_id"`
	CredentialSHA256 string `json:"credential_sha256"`
	ConversationID   string `json:"conversation_id"`
	ConversationUUID string `json:"conversation_uuid"`
	ParticipantID    int64  `json:"participant_id"`
}

type pausedRun struct {
	Turn          turnFixture `json:"turn"`
	ExecutionID   string      `json:"execution_id"`
	MessageID     string      `json:"message_id"`
	InterruptID   string      `json:"interrupt_id"`
	ToolCallID    string      `json:"tool_call_id"`
	ApproveAction string      `json:"approve_action"`
}

type heldRun struct {
	Turn        turnFixture `json:"turn"`
	ExecutionID string      `json:"execution_id"`
	MessageID   string      `json:"message_id"`
	Marker      string      `json:"marker"`
}

func (cfg config) readState(t *testing.T) state {
	t.Helper()
	evidenceMu.Lock()
	defer evidenceMu.Unlock()
	var s state
	raw, err := os.ReadFile(filepath.Join(cfg.Evidence, "state.json"))
	if os.IsNotExist(err) {
		return s
	}
	if err != nil || json.Unmarshal(raw, &s) != nil {
		t.Fatalf("state.json is unreadable: %v", err)
	}
	return s
}

func (cfg config) updateState(t *testing.T, change func(*state)) {
	t.Helper()
	s := cfg.readState(t)
	change(&s)
	evidenceMu.Lock()
	defer evidenceMu.Unlock()
	raw, _ := json.MarshalIndent(s, "", "  ")
	if err := os.WriteFile(filepath.Join(cfg.Evidence, "state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// eventually polls check until it answers true or the context ends.
func eventually(ctx context.Context, t *testing.T, every time.Duration, what string, check func() bool) {
	t.Helper()
	for {
		if check() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(every):
		}
	}
}
