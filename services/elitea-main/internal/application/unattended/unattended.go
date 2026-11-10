// Package unattended holds the rules of an unattended grant and its run chain
// (ADR-0045 decisions 1, 2 and 4, #1435, #1436).
//
// A person who starts a run can give ONE grant at the start. The grant
// answers the questions that would stop the run while nobody is there: the
// step limit, the desktop tool call cap, the output limit, a desktop hook's
// ask, a remote toolkit confirmation and named sensitive cloud tools. The
// grant never lifts a deny, a guardrail, a kill switch, a breaker, a run cap,
// a budget or a tier rule that requires a hitl node.
//
// Each automatic continuation is a new run (or local turn) in the same chain.
// The chain has a total limit on continuations, spend and wall time. Main
// checks the limits and adds the continuation in one statement
// (repos/run_chains.go), so a chain never passes its limit.
//
// A schedule or webhook run has no person at the start, so it never carries
// a grant (ADR-0041 decision 5 stays in force for it).
package unattended

import (
	"errors"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// Chain origins (run_chains.origin).
const (
	OriginCloud   = "cloud"
	OriginDesktop = "desktop"
)

// IsSystemTrigger reports a trigger origin (execution_jobs.trigger_origin)
// with no person behind it: a schedule or a webhook run.
func IsSystemTrigger(triggerOrigin string) bool {
	return triggerOrigin == "schedule" || triggerOrigin == "webhook"
}

// Link kinds (run_chain_links.kind).
const (
	LinkRoot        = "root"
	LinkStepLimit   = "step_limit"
	LinkOutputLimit = "output_limit"
	LinkToolCallCap = "tool_call_cap"
	LinkResume      = "resume"
	// LinkContinue is a desktop continuation whose host named no kind.
	LinkContinue = "continue"
	// LinkGoalNotMet is a continuation that a goal check started
	// (ADR-0045 decision 11).
	LinkGoalNotMet = "goal_not_met"
	// LinkHandoff is the first cloud run of a chain that a desktop turn
	// started: the send continues it (ADR-0045 decision 8). The chain's
	// origin becomes cloud with it. Only Main starts one; a desktop host
	// never names it.
	LinkHandoff = "handoff"
	// LinkAgentTurn is a later agent turn of a human message in a
	// conversation with several agents: another addressed agent, or one
	// handed the floor (ADR-0050 decision 7c). Only Main starts one.
	LinkAgentTurn = "agent_turn"
)

// ValidDesktopContinuationKind reports whether kind is a link kind that a
// desktop host may name on a continuation. resume: the host continues a
// chain after an app restart (ADR-0045 decision 7).
func ValidDesktopContinuationKind(kind string) bool {
	switch kind {
	case LinkStepLimit, LinkToolCallCap, LinkGoalNotMet, LinkResume:
		return true
	}
	return false
}

// Chain end states (run_chains.end_state) and the limits of limit_reached.
const (
	EndCompleted    = "completed"
	EndFailed       = "failed"
	EndWaiting      = "waiting"
	EndLimitReached = "limit_reached"
	EndCancelled    = "cancelled"

	LimitContinuations = "continuations"
	LimitSpend         = "spend"
	LimitDeadline      = "deadline"
	// The loop guards of agent-to-agent turns (ADR-0050 decision 7d,
	// turntaking.Stop*): a guard ends the human message's chain.
	LimitAgentHops        = "agent_hops"
	LimitConsecutiveTurns = "consecutive_turns"
	LimitPairExchanges    = "pair_exchanges"
)

// ValidEndState reports whether state is a chain end state.
func ValidEndState(state string) bool {
	switch state {
	case EndCompleted, EndFailed, EndWaiting, EndLimitReached, EndCancelled:
		return true
	}
	return false
}

// ContinuePrompt is the host-authored user text of a step-limit
// continuation. It is the agent runtime's CONTINUE_PROMPT
// (libs/rust/agent-runtime step_limit.rs), so the cloud and the desktop
// continue with the same words.
const ContinuePrompt = "Continue from where you stopped."

// ReasonUnattendedGrant is the ledger reason of an approval that a grant
// answered.
const ReasonUnattendedGrant = "unattended_grant"

// Refusal reasons of 422 unattended_grant_refused.
const (
	RefusedDisabled         = "disabled"
	RefusedInvalid          = "invalid"
	RefusedOverMaximum      = "over_maximum"
	RefusedNotOwner         = "not_owner"
	RefusedOrigin           = "origin_not_allowed"
	RefusedToolNotResolved  = "tool_not_resolved"
	RefusedCategoryWithheld = "category_withheld"
	RefusedTierRequiresHITL = "tier_requires_hitl"
	RefusedChainNotFound    = "chain_not_found"
)

// Run brief states (run_chains.brief_state, ADR-0045 decision 11).
const (
	// BriefNone: the grant has goal_check false (or no grant).
	BriefNone = "none"
	// BriefOff: approvals.run_brief was off when the chain started.
	BriefOff     = "off"
	BriefPending = "pending"
	BriefReady   = "ready"
	BriefFailed  = "failed"
)

// BriefStateAtStart is the brief state a new chain starts with.
func BriefStateAtStart(goalCheck, runBriefEnabled bool) string {
	switch {
	case !goalCheck:
		return BriefNone
	case !runBriefEnabled:
		return BriefOff
	}
	return BriefPending
}

// Bounds of a grant.
const (
	MaxSensitiveTools = 64
	// MaxBrowserSubmits bounds the hosts of a grant's browser_submits
	// (ADR-0049 decision 7).
	MaxBrowserSubmits  = 32
	MaxToolNameBytes   = 512
	MaxDeadlineSeconds = int64(platformconfig.MaxChainDeadlineHoursBound) * 3600
)

// Tool is one named sensitive tool: the toolkit name as the agent configures
// it, and the tool name.
type Tool struct {
	ToolkitName string `json:"toolkit_name"`
	ToolName    string `json:"tool_name"`
}

// ChainRequest is the chain part of a grant. A nil field takes the platform
// default.
type ChainRequest struct {
	MaxContinuations *int64
	MaxSpendUSD      *float64
	DeadlineSeconds  *int64
	// SpendCapUSD is a cap Main sets, never the wire (a heartbeat act's
	// daily spend left, ADR-0050 decision 2): the chain spends at most this,
	// whatever the other limits say, and it is never refused. Nil is no cap.
	SpendCapUSD *float64
}

// Request is the grant a caller sends.
type Request struct {
	// GrantID is a lowercase canonical UUID. The desktop generates it on the
	// chain's first turn; Main derives it for a cloud start.
	GrantID string
	// ChainRootExecutionID is empty on the chain's first run, and the first
	// run's execution id on every continuation.
	ChainRootExecutionID string

	ContinueStepLimit   bool
	ContinueToolCallCap bool
	ContinueOutputLimit bool
	HookAsks            bool
	RemoteConfirmations bool
	SensitiveTools      []Tool
	// BrowserSubmits are the hosts on which the run's cloud browser may
	// submit a form or upload a file without asking (ADR-0049 decision 7).
	// A consequential or payment control always asks.
	BrowserSubmits []string
	Chain          ChainRequest
	// GoalCheck asks for the run brief and the goal check (ADR-0045
	// decision 11). The wire's absent value is true.
	GoalCheck bool
	// Kind is a desktop continuation's link kind: step_limit,
	// tool_call_cap or goal_not_met. Empty is LinkContinue. Only a
	// continuation names one.
	Kind string
}

// IsContinuation reports whether the request continues a chain.
func (r Request) IsContinuation() bool { return r.ChainRootExecutionID != "" }

// OnlyContinues reports whether the request is a chain's first run whose
// grant only continues at the caps: the default "Run to completion" grant
// (ADR-0045 decision 10). It answers no question for the person. When an
// administrator turns grants off, Main starts such a run with no grant
// instead of refusing it, so the default never stops a send.
func (r Request) OnlyContinues() bool {
	return !r.IsContinuation() && !r.HookAsks && !r.RemoteConfirmations && len(r.SensitiveTools) == 0 &&
		len(r.BrowserSubmits) == 0
}

// Limits are a chain's effective limits.
type Limits struct {
	MaxContinuations int
	// MaxSpendUSD is 0 for no spend limit.
	MaxSpendUSD float64
	// Deadline is the chain's wall time from its first run.
	Deadline time.Duration
}

// Grant is the admitted grant as run_chains.grant_json stores it.
type Grant struct {
	GrantID             string `json:"grant_id"`
	ContinueStepLimit   bool   `json:"continue_step_limit"`
	ContinueToolCallCap bool   `json:"continue_tool_call_cap"`
	ContinueOutputLimit bool   `json:"continue_output_limit"`
	HookAsks            bool   `json:"hook_asks"`
	RemoteConfirmations bool   `json:"remote_confirmations"`
	SensitiveTools      []Tool `json:"sensitive_tools"`
	// BrowserSubmits are the hosts on which the cloud browser submits and
	// uploads without asking (ADR-0049 decision 7). A grant stored before
	// it has none.
	BrowserSubmits []string `json:"browser_submits,omitempty"`
	// GoalCheck: the chain has a run brief and a goal check (ADR-0045
	// decision 11). A grant stored before it has none.
	GoalCheck bool `json:"goal_check"`
	// Start is how Main starts a cloud continuation; nil for a desktop
	// chain.
	Start *CloudStart `json:"start,omitempty"`
}

// HasTool reports whether the grant names the pair, exactly.
func (g Grant) HasTool(toolkitName, toolName string) bool {
	for _, tool := range g.SensitiveTools {
		if tool.ToolkitName == toolkitName && tool.ToolName == toolName {
			return true
		}
	}
	return false
}

// CloudStart is what a step-limit continuation of a cloud chain needs to
// start the next run as the first one started. It holds no prompt and no
// secret: the model settings of an ad hoc chat are its model and its
// generation settings.
type CloudStart struct {
	// Contract is agent.execute.application.v1 (an agent) or
	// agent.execute.adhoc.v1 (a model).
	Contract      string `json:"contract"`
	ParticipantID int64  `json:"participant_id"`
	// LLMSettings is the ad hoc chat's llm_settings; empty for an agent.
	LLMSettings []byte `json:"llm_settings,omitempty"`
	// Heartbeat: a proactive agent's heartbeat started the chain (ADR-0050
	// decision 2). Every run of the chain binds only its unattended tools,
	// not only the first.
	Heartbeat bool `json:"heartbeat,omitempty"`
}

// Chain is one stored chain link and its chain.
type Chain struct {
	RootExecutionID   string
	ExecutionID       string
	ContinuationIndex int
	Kind              string
	ProjectID         int64
	OwnerUserID       int64
	ConversationUUID  string
	Origin            string
	TriggerOrigin     string
	GrantID           string
	Grant             Grant
	Limits            Limits
	DeadlineAt        time.Time
	Continuations     int
	EndState          string
	ResponseMessageID string
}

// RefusalError is a refused grant: 422 unattended_grant_refused. Field
// names the request field, Tool the toolkit/tool pair when one is refused.
type RefusalError struct {
	Field  string
	Tool   string
	Reason string
}

func (e *RefusalError) Error() string {
	text := "unattended grant refused: " + e.Reason
	if e.Field != "" {
		text += " (" + e.Field + ")"
	}
	if e.Tool != "" {
		text += " (" + e.Tool + ")"
	}
	return text
}

// AsRefusal finds a RefusalError in err's chain.
func AsRefusal(err error) (*RefusalError, bool) {
	var refusal *RefusalError
	if errors.As(err, &refusal) {
		return refusal, true
	}
	return nil, false
}

// Refused builds a RefusalError.
func Refused(field, tool, reason string) error {
	return &RefusalError{Field: field, Tool: tool, Reason: reason}
}

// IsDisabled reports whether err refuses a grant because grants are off.
func IsDisabled(err error) bool {
	var refusal *RefusalError
	return errors.As(err, &refusal) && refusal.Reason == RefusedDisabled
}

// ChainLimitError refuses a continuation: 409 chain_limit_reached. Limit is
// continuations, spend or deadline.
type ChainLimitError struct{ Limit string }

func (e *ChainLimitError) Error() string { return "chain limit reached: " + e.Limit }

// ErrChainEnded refuses a continuation of a chain that already ended for
// another reason (its last run completed, failed or was cancelled).
var ErrChainEnded = errors.New("the unattended chain has ended")

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var executionIDShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidGrantID reports whether id is a lowercase canonical UUID.
func ValidGrantID(id string) bool { return canonicalUUID.MatchString(id) }

// ValidExecutionID reports whether id has the execution id shape.
func ValidExecutionID(id string) bool { return executionIDShape.MatchString(id) }

// ValidateShape checks the parts of a request that need no setting.
func ValidateShape(request Request) error {
	if !ValidGrantID(request.GrantID) {
		return Refused("grant_id", "", RefusedInvalid)
	}
	if request.ChainRootExecutionID != "" && !ValidExecutionID(request.ChainRootExecutionID) {
		return Refused("chain_root_execution_id", "", RefusedInvalid)
	}
	if request.Kind != "" && (!request.IsContinuation() || !ValidDesktopContinuationKind(request.Kind)) {
		return Refused("kind", "", RefusedInvalid)
	}
	if len(request.SensitiveTools) > MaxSensitiveTools {
		return Refused("sensitive_tools", "", RefusedOverMaximum)
	}
	seen := map[Tool]bool{}
	for _, tool := range request.SensitiveTools {
		if !validName(tool.ToolkitName) || !validName(tool.ToolName) {
			return Refused("sensitive_tools", "", RefusedInvalid)
		}
		if seen[tool] {
			return Refused("sensitive_tools", tool.ToolkitName+"/"+tool.ToolName, RefusedInvalid)
		}
		seen[tool] = true
	}
	return ValidateBrowserSubmits(request.BrowserSubmits)
}

// ValidateBrowserSubmits checks a grant's browser_submits: at most
// MaxBrowserSubmits hosts, each a lowercase DNS host name with no scheme,
// port, path or wildcard. A repeated host is accepted;
// NormalizeBrowserSubmits removes it.
func ValidateBrowserSubmits(hosts []string) error {
	if len(hosts) > MaxBrowserSubmits {
		return Refused("browser_submits", "", RefusedOverMaximum)
	}
	for _, host := range hosts {
		if !ValidBrowserSubmitHost(host) {
			return Refused("browser_submits", "", RefusedInvalid)
		}
	}
	return nil
}

// ValidBrowserSubmitHost reports whether host is a lowercase DNS host name:
// 1 to 253 bytes, at least two labels, each label 1 to 63 bytes of a-z, 0-9
// and '-' that does not start or end with '-', and a last label that is not
// all digits (so an IPv4 literal is refused).
func ValidBrowserSubmitHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	last := labels[len(labels)-1]
	return strings.Trim(last, "0123456789") != ""
}

// NormalizeBrowserSubmits is the hosts without repeats, in their first
// order. It answers nil for none.
func NormalizeBrowserSubmits(hosts []string) []string {
	if len(hosts) == 0 {
		return nil
	}
	out := make([]string, 0, len(hosts))
	seen := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		if !seen[host] {
			seen[host] = true
			out = append(out, host)
		}
	}
	return out
}

func validName(value string) bool {
	if value == "" || len(value) > MaxToolNameBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// ResolveLimits fills the platform defaults into a chain request and
// refuses a value over the platform maximum. A max_spend that is 0, negative
// or rounds to 0 is invalid; an absent one takes the default. perRunSpendUSD is the per-run
// spend cap of the run's tier (0 for none).
func ResolveLimits(settings platformconfig.UnattendedSettings, chain ChainRequest, perRunSpendUSD float64) (Limits, error) {
	limits := Limits{
		MaxContinuations: settings.DefaultContinuations,
		Deadline:         settings.DefaultDeadline,
	}
	if chain.MaxContinuations != nil {
		value := *chain.MaxContinuations
		switch {
		case value < 0:
			return Limits{}, Refused("chain.max_continuations", "", RefusedInvalid)
		case value > int64(settings.MaxContinuations):
			return Limits{}, Refused("chain.max_continuations", "", RefusedOverMaximum)
		}
		limits.MaxContinuations = int(value)
	}
	if perRunSpendUSD > 0 {
		limits.MaxSpendUSD = perRunSpendUSD * settings.SpendMultiplier
	}
	if settings.MaxSpendUSD > 0 && (limits.MaxSpendUSD == 0 || limits.MaxSpendUSD > settings.MaxSpendUSD) {
		limits.MaxSpendUSD = settings.MaxSpendUSD
	}
	if chain.MaxSpendUSD != nil {
		value := *chain.MaxSpendUSD
		switch {
		case math.IsNaN(value) || math.IsInf(value, 0) || roundSpend(value) <= 0:
			// 0 would read as "no spend limit" in the chain row: a client
			// cannot lift the limit that way. A value that rounds to 0 is the
			// same.
			return Limits{}, Refused("chain.max_spend", "", RefusedInvalid)
		case settings.MaxSpendUSD > 0 && value > settings.MaxSpendUSD:
			return Limits{}, Refused("chain.max_spend", "", RefusedOverMaximum)
		case value > platformconfig.MaxChainSpendUSDBound:
			return Limits{}, Refused("chain.max_spend", "", RefusedOverMaximum)
		}
		limits.MaxSpendUSD = value
	}
	if spendCap := chain.SpendCapUSD; spendCap != nil && !math.IsNaN(*spendCap) && roundSpend(*spendCap) > 0 &&
		(limits.MaxSpendUSD == 0 || limits.MaxSpendUSD > *spendCap) {
		limits.MaxSpendUSD = *spendCap
	}
	limits.MaxSpendUSD = roundSpend(limits.MaxSpendUSD)
	if chain.DeadlineSeconds != nil {
		value := *chain.DeadlineSeconds
		switch {
		case value <= 0:
			return Limits{}, Refused("chain.deadline_seconds", "", RefusedInvalid)
		case value > MaxDeadlineSeconds || time.Duration(value)*time.Second > settings.MaxDeadline:
			return Limits{}, Refused("chain.deadline_seconds", "", RefusedOverMaximum)
		}
		limits.Deadline = time.Duration(value) * time.Second
	}
	return limits, nil
}

// roundSpend rounds a spend to the 4 decimals of run_chains.max_spend_usd.
func roundSpend(value float64) float64 { return math.Round(value*10000) / 10000 }

// GrantOf is the stored form of an admitted request.
func GrantOf(request Request, start *CloudStart) Grant {
	tools := append([]Tool{}, request.SensitiveTools...)
	return Grant{
		GrantID:             request.GrantID,
		ContinueStepLimit:   request.ContinueStepLimit,
		ContinueToolCallCap: request.ContinueToolCallCap,
		ContinueOutputLimit: request.ContinueOutputLimit,
		HookAsks:            request.HookAsks,
		RemoteConfirmations: request.RemoteConfirmations,
		SensitiveTools:      tools,
		BrowserSubmits:      NormalizeBrowserSubmits(request.BrowserSubmits),
		GoalCheck:           request.GoalCheck,
		Start:               start,
	}
}

// ChainStart is the first run of a chain, as the admission stores it.
type ChainStart struct {
	RootExecutionID   string
	ProjectID         int64
	OwnerUserID       int64
	ConversationUUID  string
	Origin            string
	TriggerOrigin     string
	Grant             Grant
	Limits            Limits
	ResponseMessageID string
	// BriefState is the chain's run brief state at its start
	// (BriefStateAtStart). Empty is BriefNone.
	BriefState string
}

// ChainContinue is one continuation, as the admission stores it.
type ChainContinue struct {
	RootExecutionID   string
	ExecutionID       string
	Kind              string
	OwnerUserID       int64
	GrantID           string
	ProjectID         int64
	ConversationUUID  string
	ResponseMessageID string
	// Start is set on a LinkHandoff link only: how Main starts the chain's
	// later cloud continuations. The link stores it in the chain's grant.
	Start *CloudStart
}
