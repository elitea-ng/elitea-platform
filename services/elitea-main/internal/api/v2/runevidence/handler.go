// Package runevidence serves the step records of runs (ADR-0042 decisions 1
// and 2, issue #1499) under /api/v2/elitea_core, every route behind the
// runs.evidence.read project permission:
//
//	GET /run_steps/prompt_lib/{projectID}/{runID}
//	    the run's head and a page of its steps (?after_seq=, ?limit=); the
//	    first page also carries the run's guardrail verdicts
//	GET /run_steps/prompt_lib/{projectID}/{runID}/verify
//	    recomputes the run's chain: ok, or the first seq that breaks and why
//	GET /run_steps/prompt_lib/{projectID}/{runID}/{stepID}/{inputs|outputs}
//	    one side's content, or its expiry ({expired_at, policy}); every
//	    read of stored content writes an audit_events row first
//	GET /conversation_runs/prompt_lib/{projectID}/{conversationUUID}
//	    the conversation's runs with their step counts
//
// The router applies the permission. A handler with no store answers 503.
package runevidence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runevidence"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/storage"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/pkg/apierr"
)

// Permission gates every route.
const Permission = "runs.evidence.read"

// The route patterns, relative to the /api/v2/elitea_core group.
const (
	StepsPath            = "/run_steps/prompt_lib/{projectID}/{runID}"
	VerifyPath           = StepsPath + "/verify"
	ContentPath          = StepsPath + "/{stepID}/{side}"
	ConversationRunsPath = "/conversation_runs/prompt_lib/{projectID}/{conversationUUID}"

	defaultPage = 200
	maxPage     = 1000
	maxRunID    = 128

	// maxGuardrailVerdicts bounds the guardrail verdicts on the first page.
	maxGuardrailVerdicts = 1000
)

// Store is *repos.RunSteps.
type Store interface {
	Head(ctx context.Context, projectID int64, runID string) (repos.RunHead, error)
	StepsPage(ctx context.Context, projectID int64, runID string, afterSeq int64, limit int) ([]runevidence.Record, error)
	// VerifyRun recomputes the run's chain against its head from one
	// snapshot.
	VerifyRun(ctx context.Context, projectID int64, runID string) (repos.RunVerification, error)
	Step(ctx context.Context, projectID int64, runID, stepID string) (runevidence.Record, error)
	ConversationRuns(ctx context.Context, projectID int64, conversationUUID string) ([]repos.ConversationRun, error)
	// GuardrailVerdicts reads the LLM gateway's guardrail verdicts of the run
	// (ADR-0043 decision 1), oldest first.
	GuardrailVerdicts(ctx context.Context, projectID int64, runID string, limit int) ([]repos.GuardrailVerdict, error)
	RecordContentRead(ctx context.Context, userID, projectID int64, runID, stepID string, side runevidence.Side, route string) error
}

var _ Store = (*repos.RunSteps)(nil)

// Handler serves the routes.
type Handler struct {
	store   Store
	content runevidence.ContentStore
}

// NewHandler builds the handler. A nil store (or a typed nil) answers 503; a
// nil content store answers stored content as unavailable (503).
func NewHandler(store Store, content runevidence.ContentStore) *Handler {
	if typed, ok := store.(*repos.RunSteps); ok && typed == nil {
		store = nil
	}
	return &Handler{store: store, content: content}
}

// ---------------------------------------------------------------------------
// Response shapes

// Run is a run's head in a response.
type Run struct {
	RunID            string     `json:"run_id"`
	ProjectID        int64      `json:"project_id"`
	ApplicationID    *int64     `json:"application_id"`
	ConversationUUID *string    `json:"conversation_uuid"`
	MessageID        *string    `json:"message_id"`
	Steps            int64      `json:"steps"`
	HeadHash         string     `json:"head_hash"`
	AnchoredSeq      int64      `json:"anchored_seq"`
	AnchoredAt       *time.Time `json:"anchored_at"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// ContentInfo is one content side of a step in a list: its digest and
// status, never the bytes.
type ContentInfo struct {
	Status       string  `json:"status"`
	Digest       *string `json:"digest"`
	Size         int64   `json:"size"`
	OriginalSize int64   `json:"original_size"`
	Truncated    bool    `json:"truncated"`
	Filtered     bool    `json:"filtered"`
	MediaType    string  `json:"media_type"`
}

// Step is one step record in a response.
type Step struct {
	Seq                  int64           `json:"seq"`
	StepID               string          `json:"step_id"`
	Generation           int64           `json:"generation"`
	Kind                 string          `json:"kind"`
	Actor                string          `json:"actor"`
	ReleaseID            *int64          `json:"release_id"`
	DefinitionDigest     *string         `json:"definition_digest"`
	NodeID               string          `json:"node_id"`
	Name                 string          `json:"name"`
	ToolkitID            string          `json:"toolkit_id"`
	StartedAt            time.Time       `json:"started_at"`
	EndedAt              time.Time       `json:"ended_at"`
	Summary              json.RawMessage `json:"summary"`
	Retrieval            json.RawMessage `json:"retrieval"`
	CitedStepIDs         []string        `json:"cited_step_ids"`
	CitedDocumentIDs     []string        `json:"cited_document_ids"`
	Inputs               ContentInfo     `json:"inputs"`
	Outputs              ContentInfo     `json:"outputs"`
	PrevHash             string          `json:"prev_hash"`
	Hash                 string          `json:"hash"`
	RecordedAt           time.Time       `json:"recorded_at"`
	ContentExpiredAt     *time.Time      `json:"content_expired_at"`
	ContentExpiredPolicy *string         `json:"content_expired_policy"`
}

// StepsPage is the list response.
type StepsPage struct {
	Run     Run    `json:"run"`
	Steps   []Step `json:"steps"`
	NextSeq *int64 `json:"next_after_seq"`
	// GuardrailVerdicts is set on the first page only (no after_seq).
	GuardrailVerdicts []GuardrailVerdict `json:"guardrail_verdicts,omitempty"`
}

// GuardrailVerdict is one guardrail check of an LLM request of the run
// (ADR-0043 decision 1). It never holds the inspected or the matched text.
type GuardrailVerdict struct {
	OccurredAt   time.Time `json:"occurred_at"`
	Route        string    `json:"route"`
	Stage        string    `json:"stage"`
	Row          string    `json:"row"`
	Detector     string    `json:"detector"`
	DetectorType string    `json:"detector_type"`
	Action       string    `json:"action"`
	Outcome      string    `json:"outcome"`
	FailMode     string    `json:"fail_mode"`
	Labels       []string  `json:"labels"`
	Findings     int32     `json:"findings"`
	MaxScore     float32   `json:"max_score"`
	LatencyMS    int64     `json:"latency_ms"`
	Runs         int32     `json:"runs"`
}

// Verification is the verify response.
type Verification struct {
	RunID       string             `json:"run_id"`
	OK          bool               `json:"ok"`
	Steps       int64              `json:"steps"`
	Verified    int64              `json:"verified"`
	HeadHash    string             `json:"head_hash"`
	AnchoredSeq int64              `json:"anchored_seq"`
	Break       *runevidence.Break `json:"break"`
}

// Content is the content response.
type Content struct {
	RunID     string     `json:"run_id"`
	StepID    string     `json:"step_id"`
	Side      string     `json:"side"`
	Status    string     `json:"status"`
	Digest    *string    `json:"digest"`
	MediaType string     `json:"media_type"`
	Truncated bool       `json:"truncated"`
	Filtered  bool       `json:"filtered"`
	Encoding  *string    `json:"encoding"`
	Content   *string    `json:"content"`
	ExpiredAt *time.Time `json:"expired_at"`
	Policy    *string    `json:"policy"`
}

// ConversationRun is one run in the conversation response.
type ConversationRun struct {
	RunID     string    `json:"run_id"`
	MessageID *string   `json:"message_id"`
	Steps     int64     `json:"steps"`
	Anchored  bool      `json:"anchored"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ConversationRuns is the conversation response.
type ConversationRuns struct {
	ConversationUUID string            `json:"conversation_uuid"`
	Runs             []ConversationRun `json:"runs"`
}

// ---------------------------------------------------------------------------
// Routes

// ListSteps answers the run's head and a page of its steps.
func (h *Handler) ListSteps(w http.ResponseWriter, r *http.Request) {
	projectID, runID, ok := h.run(w, r)
	if !ok {
		return
	}
	afterSeq, ok := queryInt(w, r, "after_seq", 0, 0, 1<<62)
	if !ok {
		return
	}
	limit, ok := queryInt(w, r, "limit", defaultPage, 1, maxPage)
	if !ok {
		return
	}
	head, err := h.store.Head(r.Context(), projectID, runID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	records, err := h.store.StepsPage(r.Context(), projectID, runID, afterSeq, int(limit))
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	page := StepsPage{Run: run(head), Steps: make([]Step, 0, len(records))}
	for _, record := range records {
		page.Steps = append(page.Steps, step(record))
	}
	if n := len(records); n == int(limit) && records[n-1].Seq < head.LastSeq {
		next := records[n-1].Seq
		page.NextSeq = &next
	}
	if afterSeq == 0 {
		verdicts, err := h.store.GuardrailVerdicts(r.Context(), projectID, runID, maxGuardrailVerdicts)
		if err != nil {
			writeError(r.Context(), w, err)
			return
		}
		for _, v := range verdicts {
			page.GuardrailVerdicts = append(page.GuardrailVerdicts, GuardrailVerdict{
				OccurredAt: v.OccurredAt, Route: v.Route, Stage: v.Stage, Row: v.Row, Detector: v.Detector,
				DetectorType: v.DetectorType, Action: v.Action, Outcome: v.Outcome, FailMode: v.FailMode,
				Labels: nonNil(v.Labels), Findings: v.Findings, MaxScore: v.MaxScore, LatencyMS: v.LatencyMS, Runs: v.Runs,
			})
		}
	}
	writeJSON(w, http.StatusOK, page)
}

// Verify recomputes the run's chain against its head. The head and every
// page are read in one REPEATABLE READ snapshot (repos.RunSteps.VerifyRun),
// so a run still reporting steps, or a sweep running, does not read as a
// break.
func (h *Handler) Verify(w http.ResponseWriter, r *http.Request) {
	projectID, runID, ok := h.run(w, r)
	if !ok {
		return
	}
	result, err := h.store.VerifyRun(r.Context(), projectID, runID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	writeJSON(w, http.StatusOK, Verification{
		RunID: runID, OK: result.Break == nil, Steps: result.Head.LastSeq, Verified: result.Verified,
		HeadHash: hex.EncodeToString(result.Head.LastHash), AnchoredSeq: result.Head.AnchoredSeq, Break: result.Break,
	})
}

// GetContent answers one side's content. Stored content is read, checked
// against the row's digest, and recorded in audit_events before it is
// answered; a read the trail cannot record is refused.
func (h *Handler) GetContent(w http.ResponseWriter, r *http.Request) {
	projectID, runID, ok := h.run(w, r)
	if !ok {
		return
	}
	side := runevidence.Side(chi.URLParam(r, "side"))
	if side != runevidence.SideInputs && side != runevidence.SideOutputs {
		apierr.WriteStatus(w, http.StatusBadRequest, "the side is inputs or outputs")
		return
	}
	stepID := chi.URLParam(r, "stepID")
	record, err := h.store.Step(r.Context(), projectID, runID, stepID)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	meta := record.Content(side)
	status := record.Status(side)
	answer := Content{
		RunID: runID, StepID: record.StepID, Side: string(side), Status: string(status), Digest: hexOrNil(meta.Digest),
		MediaType: meta.MediaType, Truncated: meta.Truncated, Filtered: meta.Filtered,
	}
	if status == runevidence.ContentExpired {
		answer.ExpiredAt, answer.Policy = record.ContentExpiredAt, stringOrNil(record.ContentExpiredPolicy)
	}
	if status != runevidence.ContentStored {
		writeJSON(w, http.StatusOK, answer)
		return
	}
	userID, ok := signedIn(w, r)
	if !ok {
		return
	}
	if h.content == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "step content storage is not configured")
		return
	}
	body, err := h.content.Get(r.Context(), meta.ObjectKey)
	if errors.Is(err, storage.ErrNotFound) {
		slog.ErrorContext(r.Context(), "run evidence: stored step content is missing", "run_id", runID, "step_id", record.StepID, "side", side)
		apierr.WriteStatus(w, http.StatusConflict, "the step's content is missing from storage")
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "run evidence: read step content", "run_id", runID, "error", err)
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "could not read the step's content")
		return
	}
	if digest := sha256.Sum256(body); !bytes.Equal(digest[:], meta.Digest) {
		slog.ErrorContext(r.Context(), "run evidence: stored step content does not match its digest", "run_id", runID, "step_id", record.StepID, "side", side)
		apierr.WriteStatus(w, http.StatusConflict, "the step's stored content does not match its recorded digest")
		return
	}
	route := chi.RouteContext(r.Context()).RoutePattern()
	if err := h.store.RecordContentRead(r.Context(), userID, projectID, runID, record.StepID, side, route); err != nil {
		slog.ErrorContext(r.Context(), "run evidence: content read not audited, refusing it", "run_id", runID, "error", err)
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "the read could not be recorded in the audit trail")
		return
	}
	text, encoding := string(body), "utf-8"
	if !utf8.Valid(body) {
		text, encoding = base64.StdEncoding.EncodeToString(body), "base64"
	}
	answer.Content, answer.Encoding = &text, &encoding
	writeJSON(w, http.StatusOK, answer)
}

// ListConversationRuns answers a conversation's runs, newest first.
func (h *Handler) ListConversationRuns(w http.ResponseWriter, r *http.Request) {
	projectID, ok := h.project(w, r)
	if !ok {
		return
	}
	conversation := chi.URLParam(r, "conversationUUID")
	runs, err := h.store.ConversationRuns(r.Context(), projectID, conversation)
	if err != nil {
		writeError(r.Context(), w, err)
		return
	}
	out := ConversationRuns{ConversationUUID: conversation, Runs: make([]ConversationRun, 0, len(runs))}
	for _, cr := range runs {
		out.Runs = append(out.Runs, ConversationRun{
			RunID: cr.RunID, MessageID: stringOrNil(cr.MessageID), Steps: cr.Steps, Anchored: cr.Anchored,
			StartedAt: cr.StartedAt, UpdatedAt: cr.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Helpers

func run(h repos.RunHead) Run {
	return Run{
		RunID: h.RunID, ProjectID: h.ProjectID, ApplicationID: h.ApplicationID,
		ConversationUUID: stringOrNil(h.ConversationUUID), MessageID: stringOrNil(h.MessageID),
		Steps: h.LastSeq, HeadHash: hex.EncodeToString(h.LastHash), AnchoredSeq: h.AnchoredSeq, AnchoredAt: h.AnchoredAt,
		CreatedAt: h.CreatedAt, UpdatedAt: h.UpdatedAt,
	}
}

func step(r runevidence.Record) Step {
	summary := json.RawMessage(r.Summary)
	if len(summary) == 0 {
		summary = json.RawMessage("{}")
	}
	retrieval := json.RawMessage("null")
	if r.Retrieval != nil {
		retrieval = json.RawMessage(r.Retrieval)
	}
	return Step{
		Seq: r.Seq, StepID: r.StepID, Generation: r.Generation, Kind: string(r.Kind), Actor: r.Actor,
		ReleaseID: r.ReleaseID, DefinitionDigest: hexOrNil(r.DefinitionDigest),
		NodeID: r.NodeID, Name: r.Name, ToolkitID: r.ToolkitID, StartedAt: r.StartedAt, EndedAt: r.EndedAt,
		Summary: summary, Retrieval: retrieval, CitedStepIDs: nonNil(r.CitedStepIDs), CitedDocumentIDs: nonNil(r.CitedDocumentIDs),
		Inputs: contentInfo(r, runevidence.SideInputs), Outputs: contentInfo(r, runevidence.SideOutputs),
		PrevHash: hex.EncodeToString(r.PrevHash), Hash: hex.EncodeToString(r.Hash), RecordedAt: r.RecordedAt,
		ContentExpiredAt: r.ContentExpiredAt, ContentExpiredPolicy: stringOrNil(r.ContentExpiredPolicy),
	}
}

func contentInfo(r runevidence.Record, side runevidence.Side) ContentInfo {
	m := r.Content(side)
	return ContentInfo{
		Status: string(r.Status(side)), Digest: hexOrNil(m.Digest), Size: m.Size, OriginalSize: m.OriginalSize,
		Truncated: m.Truncated, Filtered: m.Filtered, MediaType: m.MediaType,
	}
}

func (h *Handler) project(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if h == nil || h.store == nil {
		apierr.WriteStatus(w, http.StatusServiceUnavailable, "database unavailable")
		return 0, false
	}
	value, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 32)
	if err != nil || value <= 0 {
		apierr.WriteStatus(w, http.StatusBadRequest, "the path's projectID is not a valid id")
		return 0, false
	}
	return value, true
}

func (h *Handler) run(w http.ResponseWriter, r *http.Request) (int64, string, bool) {
	projectID, ok := h.project(w, r)
	if !ok {
		return 0, "", false
	}
	runID := chi.URLParam(r, "runID")
	if runID == "" || len(runID) > maxRunID {
		apierr.WriteStatus(w, http.StatusBadRequest, "the path's runID is not a valid run id")
		return 0, "", false
	}
	return projectID, runID, true
}

func queryInt(w http.ResponseWriter, r *http.Request, name string, fallback, low, high int64) (int64, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < low || value > high {
		apierr.WriteStatus(w, http.StatusBadRequest, "the "+name+" parameter is out of range")
		return 0, false
	}
	return value, true
}

func signedIn(w http.ResponseWriter, r *http.Request) (int64, bool) {
	principal, _ := auth.UserFromContext(r.Context())
	id, ok := principal.OwningUserID()
	if !ok || id <= 0 {
		apierr.WriteStatus(w, http.StatusUnauthorized, "reading step content needs a signed-in user")
		return 0, false
	}
	return id, true
}

// writeError answers ErrRunNotFound as 404 and anything else as a generic
// 500 (the cause is logged, never returned).
func writeError(ctx context.Context, w http.ResponseWriter, err error) {
	if errors.Is(err, repos.ErrRunNotFound) {
		apierr.WriteStatus(w, http.StatusNotFound, "no step records for this run in this project")
		return
	}
	slog.ErrorContext(ctx, "run evidence", "error", err)
	apierr.WriteStatus(w, http.StatusInternalServerError, "could not read the run's step records")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func hexOrNil(b []byte) *string {
	if b == nil {
		return nil
	}
	s := hex.EncodeToString(b)
	return &s
}

func stringOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
