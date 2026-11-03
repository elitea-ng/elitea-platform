package runbrief

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
)

// CallTimeout bounds each brief and each check call.
const CallTimeout = 20 * time.Second

// ClaimLapse is how long one replica's claim on a pending brief holds: only
// the claimant asks the model, and a claim older than this lapses so another
// call can retry after a crash.
const ClaimLapse = 30 * time.Second

// MaxBriefAttempts bounds the model calls for one chain's brief in one
// process: a transport error leaves the brief pending for a later
// EnsureBrief, and the last attempt's error freezes it failed.
const MaxBriefAttempts = 3

// ErrNoModelPlane is a call before the model plane is composed (or on a
// deployment without one). It never freezes a brief and is not an attempt.
var ErrNoModelPlane = errors.New("run brief: no LLM gateway is composed")

// Output bounds of the two calls, in tokens.
const (
	briefMaxTokens = 1200
	checkMaxTokens = 1500
)

// Attribution prefixes: each call is signed with one as its execution id, so
// the gateway's request log row names the chain link it belongs to and the
// chain spend (repos.continueRunChainSQL) counts it. The per-run spend cap
// reads the run's own execution id only, so these calls never count there.
const (
	AttributionBriefPrefix = "run_brief:"
	AttributionCheckPrefix = "goal_check:"
)

// CompletionRequest is one blocking model call.
type CompletionRequest struct {
	// ProjectID and UserID are the chain's project and owner: the gateway
	// meters and authorizes the call against them.
	ProjectID int64
	UserID    int64
	// Model is the project's low-tier model; empty takes the project's
	// default model.
	Model         string
	System        string
	User          string
	AttributionID string
	MaxTokens     int
}

// Completer is the model call. The composition root adapts the predict
// completer (elitea-llm-gateway).
type Completer interface {
	Complete(ctx context.Context, request CompletionRequest) (string, error)
}

// ModelResolver names the project's low-tier default model, or "" when the
// project has none.
type ModelResolver interface {
	LowTierModel(ctx context.Context, projectID int64) (string, error)
}

// Settings reports approvals.run_brief.
type Settings interface {
	RunBriefEnabled(ctx context.Context) (bool, error)
}

// ChainBrief is a chain's brief state.
type ChainBrief struct {
	RootExecutionID string
	ProjectID       int64
	OwnerUserID     int64
	Origin          string
	State           string
	Brief           *Brief
	// Failures is condition id -> consecutive not_met checks.
	Failures map[string]int
	// LastCheck is the chain's latest goal check; nil before the first.
	LastCheck *Result
}

// Store reads and writes the brief and the checks (repos.RunChainsRepository).
type Store interface {
	ChainBrief(ctx context.Context, root string) (ChainBrief, bool, error)
	// ClaimBrief claims a pending brief for this replica: true when no other
	// replica holds a claim younger than ClaimLapse.
	ClaimBrief(ctx context.Context, root string) (bool, error)
	// ReleaseBrief drops the claim on a brief that stays pending.
	ReleaseBrief(ctx context.Context, root string) error
	// SaveBrief moves a pending brief to state (ready with brief, failed or
	// off) and answers the stored state. It changes nothing when the brief
	// is not pending.
	SaveBrief(ctx context.Context, root, state string, brief *Brief) (ChainBrief, error)
	// LinkCheck answers whether executionID is a link of root, and its
	// stored check.
	LinkCheck(ctx context.Context, root, executionID string) (*Result, bool, error)
	// RecordCheck writes a link's check and the chain's counts once, and
	// answers the stored check.
	RecordCheck(ctx context.Context, root, executionID string, result Result, failures map[string]int) (Result, error)
}

// Transcripts reads the cloud side of a chain.
type Transcripts interface {
	// RootRequest is the person's first message of a cloud chain.
	RootRequest(ctx context.Context, root string) (string, error)
	// Transcript is the chain's transcript up to now, oldest first.
	Transcript(ctx context.Context, root string) (string, error)
}

// ErrNotLink refuses a check of an execution that is not a link of the chain.
var ErrNotLink = errors.New("runbrief: the execution is not a link of the chain")

// Service writes briefs and runs goal checks.
type Service struct {
	completer   Completer
	models      ModelResolver
	settings    Settings
	store       Store
	transcripts Transcripts
	logger      *slog.Logger

	// pollEvery is how often a call that lost the claim reads the brief.
	pollEvery time.Duration

	mu       sync.Mutex
	inflight map[string]chan struct{}
	// attempts is root -> the brief calls that failed with a transport
	// error in this process.
	attempts map[string]int
}

// New builds the service. It answers nil when a part is missing.
func New(
	completer Completer, models ModelResolver, settings Settings, store Store, transcripts Transcripts,
	logger *slog.Logger,
) *Service {
	if completer == nil || models == nil || settings == nil || store == nil || transcripts == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		completer: completer, models: models, settings: settings, store: store, transcripts: transcripts,
		logger: logger, pollEvery: 250 * time.Millisecond,
		inflight: map[string]chan struct{}{}, attempts: map[string]int{},
	}
}

// Begin writes a new cloud chain's brief in the background. It never blocks
// the caller.
func (s *Service) Begin(ctx context.Context, root, request string) {
	if s == nil {
		return
	}
	background := context.WithoutCancel(ctx)
	go func() {
		if _, _, err := s.EnsureBrief(background, root, request); err != nil {
			s.logger.ErrorContext(background, "run brief: write", "chain_root_execution_id", root, "err", err)
		}
	}()
}

// Chain reads a chain's brief state without writing anything.
func (s *Service) Chain(ctx context.Context, root string) (ChainBrief, bool, error) {
	return s.store.ChainBrief(ctx, root)
}

// EnsureBrief answers the chain's brief, and writes it first when it is
// pending. request is the person's first message; empty reads it from a
// cloud chain's transcript. A brief that another call in this process is
// writing is waited for; a brief that another replica claimed is polled for
// up to CallTimeout. A brief that stays pending (a transport error, another
// replica still writing it) is answered pending: the caller has no check
// this time.
func (s *Service) EnsureBrief(ctx context.Context, root, request string) (ChainBrief, bool, error) {
	chain, found, err := s.store.ChainBrief(ctx, root)
	if err != nil || !found || chain.State != unattended.BriefPending {
		return chain, found, err
	}
	done, owner := s.claim(root)
	if !owner {
		select {
		case <-done:
		case <-ctx.Done():
			return chain, true, ctx.Err()
		}
		chain, found, err = s.store.ChainBrief(ctx, root)
		return chain, found, err
	}
	defer s.release(root, done)
	chain, err = s.writeBrief(ctx, chain, request)
	return chain, true, err
}

func (s *Service) claim(root string) (chan struct{}, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if done, ok := s.inflight[root]; ok {
		return done, false
	}
	done := make(chan struct{})
	s.inflight[root] = done
	return done, true
}

func (s *Service) release(root string, done chan struct{}) {
	s.mu.Lock()
	delete(s.inflight, root)
	s.mu.Unlock()
	close(done)
}

// writeBrief asks the model and stores the brief once.
func (s *Service) writeBrief(ctx context.Context, chain ChainBrief, request string) (ChainBrief, error) {
	enabled, err := s.settings.RunBriefEnabled(ctx)
	if err != nil {
		return chain, fmt.Errorf("run brief: read approvals.run_brief: %w", err)
	}
	if !enabled {
		return s.store.SaveBrief(ctx, chain.RootExecutionID, unattended.BriefOff, nil)
	}
	if request == "" && chain.Origin == unattended.OriginCloud {
		if request, err = s.transcripts.RootRequest(ctx, chain.RootExecutionID); err != nil {
			return chain, err
		}
	}
	if request == "" {
		// Nothing to brief from: the desktop host sends the request.
		return chain, nil
	}
	claimed, err := s.store.ClaimBrief(ctx, chain.RootExecutionID)
	if err != nil {
		return chain, err
	}
	if !claimed {
		// Another replica asks the model.
		return s.awaitBrief(ctx, chain)
	}
	brief, err := s.generate(ctx, chain, request)
	if err == nil {
		s.forget(chain.RootExecutionID)
		return s.store.SaveBrief(ctx, chain.RootExecutionID, unattended.BriefReady, &brief)
	}
	if s.final(chain.RootExecutionID, err) {
		s.logger.WarnContext(ctx, "run brief: no brief", "chain_root_execution_id", chain.RootExecutionID, "err", err)
		s.forget(chain.RootExecutionID)
		return s.store.SaveBrief(ctx, chain.RootExecutionID, unattended.BriefFailed, nil)
	}
	s.logger.WarnContext(ctx, "run brief: not written yet; a later call retries",
		"chain_root_execution_id", chain.RootExecutionID, "err", err)
	if err := s.store.ReleaseBrief(context.WithoutCancel(ctx), chain.RootExecutionID); err != nil {
		s.logger.WarnContext(ctx, "run brief: release the claim", "chain_root_execution_id", chain.RootExecutionID,
			"err", err)
	}
	return chain, nil
}

// final reports whether a brief error freezes the brief failed: an invalid
// answer, the call timeout, or the last of MaxBriefAttempts transport
// errors. ErrNoModelPlane never does.
func (s *Service) final(root string, err error) bool {
	switch {
	case errors.Is(err, ErrNoModelPlane):
		return false
	case errors.Is(err, ErrInvalidBrief), errors.Is(err, context.DeadlineExceeded):
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts[root]++
	return s.attempts[root] >= MaxBriefAttempts
}

func (s *Service) forget(root string) {
	s.mu.Lock()
	delete(s.attempts, root)
	s.mu.Unlock()
}

// awaitBrief polls a brief that another replica claimed until it is no
// longer pending, for up to CallTimeout, and answers the last read.
func (s *Service) awaitBrief(ctx context.Context, chain ChainBrief) (ChainBrief, error) {
	deadline := time.NewTimer(CallTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(s.pollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return chain, ctx.Err()
		case <-deadline.C:
			return chain, nil
		case <-tick.C:
		}
		next, found, err := s.store.ChainBrief(ctx, chain.RootExecutionID)
		if err != nil {
			return chain, err
		}
		if !found {
			return chain, nil
		}
		chain = next
		if chain.State != unattended.BriefPending {
			return chain, nil
		}
	}
}

func (s *Service) generate(ctx context.Context, chain ChainBrief, request string) (Brief, error) {
	answer, err := s.call(ctx, chain, AttributionBriefPrefix+chain.RootExecutionID,
		briefSystemPrompt, briefUserPrompt(request), briefMaxTokens)
	if err != nil {
		return Brief{}, err
	}
	return ParseBrief(answer)
}

// call makes one bounded model call on the project's low-tier model, else
// its default model.
func (s *Service) call(
	ctx context.Context, chain ChainBrief, attribution, system, user string, maxTokens int,
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	model, err := s.models.LowTierModel(ctx, chain.ProjectID)
	if err != nil {
		s.logger.WarnContext(ctx, "run brief: low-tier model unreadable; the default model answers",
			"project_id", chain.ProjectID, "err", err)
		model = ""
	}
	return s.completer.Complete(ctx, CompletionRequest{
		ProjectID: chain.ProjectID, UserID: chain.OwnerUserID, Model: model,
		System: system, User: user, AttributionID: attribution, MaxTokens: maxTokens,
	})
}

// ContinueText is the user text of a continuation of root: the brief block
// when the chain has a ready brief, else unattended.ContinuePrompt.
func (s *Service) ContinueText(ctx context.Context, root string) string {
	if s == nil {
		return unattended.ContinuePrompt
	}
	chain, found, err := s.store.ChainBrief(ctx, root)
	if err != nil || !found || chain.State != unattended.BriefReady {
		return unattended.ContinuePrompt
	}
	return ContinuationText(chain.Brief, chain.LastCheck)
}

// Check runs the goal check of a chain's run that would end the chain as
// completed. transcript reads the chain's transcript; it is called only
// when there is something to check. It answers false when the chain has no
// check (no ready brief, no conditions, or approvals.run_brief off): the
// chain then ends as before. A replay answers the stored check.
func (s *Service) Check(
	ctx context.Context, root, executionID string, transcript func(context.Context) (string, error),
) (Result, bool, error) {
	if s == nil {
		return Result{}, false, nil
	}
	stored, isLink, err := s.store.LinkCheck(ctx, root, executionID)
	if err != nil {
		return Result{}, false, err
	}
	if !isLink {
		return Result{}, false, ErrNotLink
	}
	if stored != nil {
		return *stored, true, nil
	}
	chain, found, err := s.EnsureBrief(ctx, root, "")
	if err != nil {
		return s.notChecked(ctx, root, executionID, err)
	}
	if !found || chain.State != unattended.BriefReady || !chain.Brief.HasConditions() {
		return Result{}, false, nil
	}
	enabled, err := s.settings.RunBriefEnabled(ctx)
	if err != nil {
		return s.notChecked(ctx, root, executionID, fmt.Errorf("read approvals.run_brief: %w", err))
	}
	if !enabled {
		return Result{}, false, nil
	}
	// A concurrent replay may have checked this run while the brief was
	// written: its check stands and the model is not asked again.
	if stored, _, err := s.store.LinkCheck(ctx, root, executionID); err == nil && stored != nil {
		return *stored, true, nil
	}
	result, failures := s.judge(ctx, chain, executionID, transcript)
	recorded, err := s.store.RecordCheck(ctx, root, executionID, result, failures)
	if err != nil {
		return s.notChecked(ctx, root, executionID, fmt.Errorf("record the check: %w", err))
	}
	return recorded, true, nil
}

// notChecked is a check that an infrastructure error stopped: the run is
// "not checked" with the counts unchanged, recorded when the store answers.
// Without a ready brief that has conditions there is nothing to record, and
// the error stays for the caller (the settle hook ends the chain completed).
func (s *Service) notChecked(ctx context.Context, root, executionID string, cause error) (Result, bool, error) {
	s.logger.WarnContext(ctx, "goal check: not checked", "chain_root_execution_id", root,
		"execution_id", executionID, "err", cause)
	chain, found, err := s.store.ChainBrief(ctx, root)
	if err != nil || !found || chain.State != unattended.BriefReady || !chain.Brief.HasConditions() {
		return Result{}, false, cause
	}
	result := NotChecked(chain.Brief)
	recorded, err := s.store.RecordCheck(ctx, root, executionID, result, chain.Failures)
	if err != nil {
		s.logger.WarnContext(ctx, "goal check: record not checked", "chain_root_execution_id", root,
			"execution_id", executionID, "err", err)
		return result, true, nil
	}
	return recorded, true, nil
}

// CheckCloud is Check with the cloud chain's own transcript.
func (s *Service) CheckCloud(ctx context.Context, root, executionID string) (Result, bool, error) {
	if s == nil {
		return Result{}, false, nil
	}
	return s.Check(ctx, root, executionID, func(ctx context.Context) (string, error) {
		return s.transcripts.Transcript(ctx, root)
	})
}

// judge asks the model and decides. A failure is not_checked, with the
// counts unchanged.
func (s *Service) judge(
	ctx context.Context, chain ChainBrief, executionID string, transcript func(context.Context) (string, error),
) (Result, map[string]int) {
	text, err := transcript(ctx)
	if err == nil {
		var answer string
		answer, err = s.call(ctx, chain, AttributionCheckPrefix+executionID, checkSystemPrompt,
			checkUserPrompt(*chain.Brief, text), checkMaxTokens)
		if err == nil {
			var verdicts []ConditionResult
			if verdicts, err = ParseVerdicts(answer, *chain.Brief); err == nil {
				result, failures := Decide(verdicts, chain.Failures)
				if result.Outcome == OutcomeNotMet {
					result.ContinueText = ContinuationText(chain.Brief, &result)
				}
				return result, failures
			}
		}
	}
	s.logger.WarnContext(ctx, "goal check: not checked", "chain_root_execution_id", chain.RootExecutionID,
		"execution_id", executionID, "err", err)
	return NotChecked(chain.Brief), chain.Failures
}
