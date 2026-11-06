// Package browserruntime builds the runtime-context `browser` field (ADR-0049
// decisions 3, 5 and 8, #1596): the cloud browser's engine, its egress
// through the egress proxy, its run and monthly caps, and whether the run is
// unattended.
//
// Main serves the field only to a worker that lists `browser` in its accept
// fields, and only when the project's `browser` setting is enabled. The
// browser's only route out is the egress proxy, so the egress mode is
// `proxy` or `unavailable`, never `direct` or `none`.
package browserruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/libs/go/egresslib"
	"github.com/elitea-ng/elitea-platform/libs/go/egresslib/runtoken"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/unattended"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/workspaceruntime"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// RuntimeContextField is the accept-field name and the JSON key.
const RuntimeContextField = "browser"

// EngineFull is the one engine (configurations.BrowserEngineFull).
const EngineFull = "full"

// The egress modes of the browser field. The browser has no direct route.
const (
	EgressModeProxy       = workspaceruntime.EgressModeProxy
	EgressModeUnavailable = workspaceruntime.EgressModeUnavailable
)

// Why the egress is unavailable: the workspace runtime's fixed strings, and
// the browser's own. The worker admits each one (browser_run/context.rs).
const (
	EgressUnavailableProxyNotEnabled = workspaceruntime.EgressUnavailableProxyNotEnabled
	EgressUnavailableNoSigningKey    = workspaceruntime.EgressUnavailableNoSigningKey
	EgressUnavailableMintFailed      = workspaceruntime.EgressUnavailableMintFailed
	// EgressUnavailableAllowlistTooLarge: the merged allowlist has more
	// entries than one run token carries.
	EgressUnavailableAllowlistTooLarge = "browser_allowlist_too_large"
	// EgressUnavailableAllowlistInvalid: a stored allowlist entry does not
	// parse (a row written past the validation).
	EgressUnavailableAllowlistInvalid = "browser_allowlist_invalid"
	// EgressUnavailableSourceFailed: a source of the field (the platform
	// section, the trigger origin, the month's usage) could not be read.
	EgressUnavailableSourceFailed = "browser_context_unavailable"
)

// DefaultTokenWall is the token lifetime base when max_run_seconds is 0.
const DefaultTokenWall = time.Hour

// maxTokenTTL is the verifier's longest token.
const maxTokenTTL = runtoken.MaxLifetime

// MaxAllowlistEntries is the run token's bound on one allowlist.
const MaxAllowlistEntries = runtoken.MaxEntries

// Errors of the field's sources. ResolveBrowserContext answers only
// ErrInvalidClaim and ErrUnavailable (the project setting could not be read);
// every other one degrades the field to egress `unavailable` with a fixed
// reason, so a redemption never fails for the browser.
var (
	ErrInvalidClaim = errors.New("browserruntime: the claim is not valid")
	ErrUnavailable  = errors.New("browserruntime: a source of the browser field is unavailable")
	// ErrAllowlistTooLarge: the assembled allowlist has more entries than
	// the run token admits.
	ErrAllowlistTooLarge = errors.New("browserruntime: the browser allowlist has too many entries")
	// ErrAllowlistInvalid: a stored entry does not parse in the egresslib
	// grammar (a row written past the validation).
	ErrAllowlistInvalid = errors.New("browserruntime: a stored browser allowlist entry is not valid")
)

// Claim is the claimed execution the field is built for.
type Claim struct {
	ExecutionID string
	ProjectID   int64
	// Governed: the run is pinned to the governed credential profile. Its
	// token always has an allowlist.
	Governed bool
}

// ProjectSetting is the project's `browser` configuration row.
type ProjectSetting struct {
	Enabled      bool     `json:"enabled"`
	Engine       string   `json:"engine"`
	AllowDomains []string `json:"allow_domains"`
}

// PlatformReader reads the platform sections.
type PlatformReader interface {
	BrowserSettings(ctx context.Context) (platformconfig.BrowserSettings, error)
	WorkerEgressSettings(ctx context.Context) (platformconfig.WorkerEgressSettings, error)
}

// ProjectReader reads the project settings. A project with no row answers
// the zero setting and no entries.
type ProjectReader interface {
	BrowserProjectSetting(ctx context.Context, projectID int64) (ProjectSetting, error)
	WorkerEgressProjectEntries(ctx context.Context, projectID int64) ([]string, error)
}

// UsageReader sums the settled browser engine seconds of one project since
// a time.
type UsageReader interface {
	BrowserSecondsSince(ctx context.Context, projectID int64, since time.Time) (int64, error)
}

// TriggerReader reads who is behind an execution: its trigger origin
// (execution_jobs.trigger_origin), and whether it runs under an ADR-0045
// unattended grant (a run_chain_links row: the grant's root run or one of
// its continuations).
type TriggerReader interface {
	TriggerOrigin(ctx context.Context, executionID string) (string, error)
	UnattendedGrant(ctx context.Context, executionID string) (bool, error)
}

// Minter signs egress run tokens (egresstoken.Minter).
type Minter interface {
	Mint(executionID string, projectID int64, allowlist []string, ttl time.Duration) (string, error)
	MintOpen(executionID string, projectID int64, ttl time.Duration) (string, error)
}

// Egress is the deployment's egress proxy: the workspace runtime's proxy URL
// and minter. An empty ProxyURL means the proxy is not enabled; a nil Minter
// means no signing key is configured.
type Egress struct {
	ProxyURL string
	Minter   Minter
}

// RuntimeContext is the `browser` field. Every key is always present: the
// worker parses it with `deny_unknown_fields` and requires every key.
type RuntimeContext struct {
	Engine              string                                `json:"engine"`
	Egress              workspaceruntime.RuntimeContextEgress `json:"egress"`
	MaxRunSeconds       int64                                 `json:"max_run_seconds"`
	MonthlyQuotaSeconds int64                                 `json:"monthly_quota_seconds"`
	MonthUsedSeconds    int64                                 `json:"month_used_seconds"`
	Unattended          bool                                  `json:"unattended"`
	// AllowDomains is the project's own list, for the worker's step record
	// only. The proxy enforces the token, not this list.
	AllowDomains []string `json:"allow_domains"`
}

// Resolver builds the field.
type Resolver struct {
	platform PlatformReader
	project  ProjectReader
	usage    UsageReader
	trigger  TriggerReader
	egress   Egress
	logger   *slog.Logger
	now      func() time.Time
}

// NewResolver returns nil when a reader is nil: the composition root then
// serves no `browser` field.
func NewResolver(platform PlatformReader, project ProjectReader, usage UsageReader, trigger TriggerReader, egress Egress, logger *slog.Logger) *Resolver {
	if platform == nil || project == nil || usage == nil || trigger == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Resolver{platform: platform, project: project, usage: usage, trigger: trigger, egress: egress, logger: logger, now: time.Now}
}

// WithClock replaces time.Now, for tests.
func (r *Resolver) WithClock(now func() time.Time) *Resolver {
	if r != nil && now != nil {
		r.now = now
	}
	return r
}

// ResolveBrowserContext returns nil and no error when the project does not
// enable the cloud browser. It answers an error only for an invalid claim or
// a project setting it could not read (the caller serves no field then).
// Every later failure degrades: the field is served with egress
// `unavailable` and a fixed reason, so the browser tools answer
// `browser_unavailable` and the run itself still starts. The log line names
// the execution, the project, the stage and the error, never a token.
func (r *Resolver) ResolveBrowserContext(ctx context.Context, claim Claim) (*RuntimeContext, error) {
	if r == nil {
		return nil, nil
	}
	if claim.ExecutionID == "" || claim.ProjectID <= 0 {
		return nil, ErrInvalidClaim
	}
	setting, err := r.project.BrowserProjectSetting(ctx, claim.ProjectID)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	if !setting.Enabled {
		return nil, nil
	}
	domains := append([]string{}, setting.AllowDomains...)
	if setting.Engine != "" && setting.Engine != EngineFull {
		// A stored engine nobody admits: the browser stays off for the run.
		r.logger.WarnContext(ctx, "browser context: the project names an engine that is not admitted; the browser is unavailable",
			"execution_id", claim.ExecutionID, "project_id", claim.ProjectID, "engine", setting.Engine)
		return degraded(platformconfig.DefaultBrowserSettings(), true, domains, EgressUnavailableSourceFailed), nil
	}
	platform, err := r.platform.BrowserSettings(ctx)
	if err != nil {
		return r.degrade(ctx, claim, "browser_settings", err, platform, true, domains), nil
	}
	// Unattended: nobody can answer. A schedule or webhook run, or a run a
	// person started under an unattended grant (ADR-0045): either way its
	// asks follow the grant and the run's on_approval_required, and
	// browser_request_user answers no_person. A run whose origin cannot be
	// read is degraded as unattended (the narrower reading).
	origin, err := r.trigger.TriggerOrigin(ctx, claim.ExecutionID)
	if err != nil {
		return r.degrade(ctx, claim, "trigger_origin", err, platform, true, domains), nil
	}
	runUnattended := unattended.IsSystemTrigger(origin)
	if !runUnattended {
		granted, err := r.trigger.UnattendedGrant(ctx, claim.ExecutionID)
		if err != nil {
			return r.degrade(ctx, claim, "unattended_grant", err, platform, true, domains), nil
		}
		runUnattended = granted
	}
	monthStart := startOfMonthUTC(r.now())
	used, err := r.usage.BrowserSecondsSince(ctx, claim.ProjectID, monthStart)
	if err != nil {
		// Without the month's usage the quota cannot be held: no browser.
		return r.degrade(ctx, claim, "month_usage", err, platform, runUnattended, domains), nil
	}
	egress := r.egressFor(ctx, claim, platform, setting)
	return &RuntimeContext{
		Engine:              EngineFull,
		Egress:              egress,
		MaxRunSeconds:       platform.MaxRunSeconds,
		MonthlyQuotaSeconds: platform.MonthlyQuotaSeconds,
		MonthUsedSeconds:    max(used, 0),
		Unattended:          runUnattended,
		AllowDomains:        domains,
	}, nil
}

// degrade logs a source failure and answers the field with egress
// `unavailable`. The error text is the source's (a database error names no
// token).
func (r *Resolver) degrade(ctx context.Context, claim Claim, stage string, err error, platform platformconfig.BrowserSettings, runUnattended bool, domains []string) *RuntimeContext {
	r.logger.WarnContext(ctx, "browser context: a source could not be read; the browser is unavailable for this run",
		"execution_id", claim.ExecutionID, "project_id", claim.ProjectID, "stage", stage,
		"reason", EgressUnavailableSourceFailed, "error", err.Error())
	return degraded(platform, runUnattended, domains, EgressUnavailableSourceFailed)
}

func degraded(platform platformconfig.BrowserSettings, runUnattended bool, domains []string, reason string) *RuntimeContext {
	return &RuntimeContext{
		Engine:              EngineFull,
		Egress:              unavailable(reason),
		MaxRunSeconds:       platform.MaxRunSeconds,
		MonthlyQuotaSeconds: platform.MonthlyQuotaSeconds,
		Unattended:          runUnattended,
		AllowDomains:        domains,
	}
}

// egressFor decides the egress. Open mints an open token only when the
// platform says open and the run is not governed; else the token carries
// the assembled allowlist.
//
// An allowlist that cannot be assembled is egress `unavailable`, never a
// failure: too many entries for one run token
// (EgressUnavailableAllowlistTooLarge), a stored entry that does not parse
// (EgressUnavailableAllowlistInvalid), or a source read error.
func (r *Resolver) egressFor(ctx context.Context, claim Claim, platform platformconfig.BrowserSettings, setting ProjectSetting) workspaceruntime.RuntimeContextEgress {
	open := platform.Egress == platformconfig.BrowserEgressOpen && !claim.Governed
	var allowlist []string
	if !open {
		var err error
		allowlist, err = r.allowlist(ctx, claim.ProjectID, setting)
		if err != nil {
			reason := EgressUnavailableSourceFailed
			switch {
			case errors.Is(err, ErrAllowlistTooLarge):
				reason = EgressUnavailableAllowlistTooLarge
			case errors.Is(err, ErrAllowlistInvalid):
				reason = EgressUnavailableAllowlistInvalid
			}
			r.logger.WarnContext(ctx, "browser egress: the allowlist could not be assembled; the browser cannot reach the network",
				"execution_id", claim.ExecutionID, "project_id", claim.ProjectID, "reason", reason,
				"max_entries", MaxAllowlistEntries, "error", err.Error())
			return unavailable(reason)
		}
	}
	switch {
	case r.egress.ProxyURL == "":
		r.logger.WarnContext(ctx, "browser egress: no egress proxy is enabled; the browser cannot reach the network",
			"execution_id", claim.ExecutionID, "project_id", claim.ProjectID)
		return unavailable(EgressUnavailableProxyNotEnabled)
	case r.egress.Minter == nil:
		r.logger.WarnContext(ctx, "browser egress: no run-token signing key is configured; the browser cannot reach the network",
			"execution_id", claim.ExecutionID, "project_id", claim.ProjectID)
		return unavailable(EgressUnavailableNoSigningKey)
	}
	ttl := tokenTTL(platform.MaxRunSeconds)
	var (
		token string
		err   error
	)
	switch {
	case open:
		token, err = r.egress.Minter.MintOpen(claim.ExecutionID, claim.ProjectID, ttl)
	case len(allowlist) == 0:
		err = errors.New("the browser allowlist is empty")
	default:
		token, err = r.egress.Minter.Mint(claim.ExecutionID, claim.ProjectID, allowlist, ttl)
	}
	if err != nil {
		// The mint error names no token material.
		r.logger.ErrorContext(ctx, "browser egress: the run token could not be minted; the browser cannot reach the network",
			"execution_id", claim.ExecutionID, "project_id", claim.ProjectID, "error", err.Error())
		return unavailable(EgressUnavailableMintFailed)
	}
	return workspaceruntime.RuntimeContextEgress{
		Mode: EgressModeProxy, ProxyURL: canonicalProxyURL(r.egress.ProxyURL), Token: token,
	}
}

// allowlist assembles the platform worker egress allowlist, the project's
// worker egress entries and the project's browser domains, in that order,
// without duplicates. A stored entry that does not parse in the egresslib
// grammar fails the field. An entry the run-token grammar cannot carry (a
// CIDR block, a private IP literal) is left out: the proxy never admits it,
// so leaving it out only narrows.
func (r *Resolver) allowlist(ctx context.Context, projectID int64, setting ProjectSetting) ([]string, error) {
	platform, err := r.platform.WorkerEgressSettings(ctx)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	project, err := r.project.WorkerEgressProjectEntries(ctx, projectID)
	if err != nil {
		return nil, errors.Join(ErrUnavailable, err)
	}
	return AssembleAllowlist(platform.Allowlist, project, setting.AllowDomains)
}

// AssembleAllowlist is the deduplicated union of the lists, normalised in
// the run-token grammar. See Resolver.allowlist.
func AssembleAllowlist(lists ...[]string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, list := range lists {
		for _, raw := range list {
			if _, err := egresslib.ParseEntry(raw); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrAllowlistInvalid, err)
			}
			rule, err := runtoken.ParseRule(raw)
			if err != nil {
				continue
			}
			entry := rule.String()
			if seen[entry] {
				continue
			}
			seen[entry] = true
			out = append(out, entry)
		}
	}
	if len(out) > MaxAllowlistEntries {
		return nil, ErrAllowlistTooLarge
	}
	return out, nil
}

func unavailable(reason string) workspaceruntime.RuntimeContextEgress {
	return workspaceruntime.RuntimeContextEgress{Mode: EgressModeUnavailable, Reason: reason}
}

// tokenTTL is the run cap (1 h when there is none) plus the workspace
// egress grace, at most 24 h.
func tokenTTL(maxRunSeconds int64) time.Duration {
	wall := DefaultTokenWall
	if maxRunSeconds > 0 && maxRunSeconds < int64(maxTokenTTL/time.Second) {
		wall = time.Duration(maxRunSeconds) * time.Second
	}
	return min(wall+workspaceruntime.EgressTokenGrace, maxTokenTTL)
}

// canonicalProxyURL is the URL as served, `http://host:port`. The
// composition root validated it with workspaceruntime.ValidateProxyURL.
func canonicalProxyURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return raw
	}
	return "http://" + parsed.Host
}

func startOfMonthUTC(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
}
