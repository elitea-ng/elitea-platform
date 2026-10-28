// Package runmandate decides whether an unattended governed run may start
// under the mandate attached to it (ADR-0039, issue #1122).
//
// Three entry points start a run nobody is watching: a pipeline schedule,
// an inbound pipeline trigger and an index schedule. With credential
// governance on, none of them runs as a person's identity or a project
// system token. Each carries an attached mandate, and this package is the
// one place that answers, at attach time and again at every fire:
//
//   - the mandate exists, is in the target's tenant project and is of a
//     kind the active policy revision allows for unattended runs
//     (autonomous or delegated_background, gate D4; none by default);
//   - its agent binding is usable and, for a pipeline, binds the version
//     the schedule or trigger starts;
//   - at fire time it is active, not expired, and the epochs of the
//     mandate, its binding and its delegation are current (#1121);
//   - its resource scopes still cover the schedule's cron and input.
//
// A fire that passes returns the securitycontext.RunAuthority the admission
// writes into the run's security context. A fire that fails is skipped by
// the caller, and Refuse records a security audit decision event (#1114).
//
// # Resource scopes this package reads
//
// A mandate's resource_scopes may restrict what an unattended run does. Two
// prefixes are read here; any other scope is left to the credential
// decision:
//
//	schedule.cron:<expression>        the cron the run may fire on, with the
//	                                  five fields joined by "_" (scopes hold
//	                                  no whitespace): schedule.cron:0_3_*_*_*
//	schedule.input:sha256:<hex>       the SHA-256 of the input it may run with
//
// When a prefix is present, the target must match one of its scopes. When
// it is absent, that dimension is not restricted.
package runmandate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialbroker/epoch"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/runtime/securitycontext"
)

// Origin names the unattended entry point.
type Origin string

const (
	OriginPipelineSchedule Origin = "pipeline_schedule"
	OriginPipelineTrigger  Origin = "pipeline_trigger"
	OriginIndexSchedule    Origin = "index_schedule"
)

// The result codes callers report. They are API and settings-tab
// vocabulary, so they never change once shipped.
const (
	CodeMandateRequired        = "MANDATE_REQUIRED"
	CodeMandateNotFound        = "MANDATE_NOT_FOUND"
	CodeMandateNotUsable       = "MANDATE_NOT_USABLE"
	CodeMandateScopeMismatch   = "MANDATE_SCOPE_MISMATCH"
	CodeMandateTenantMismatch  = "MANDATE_TENANT_MISMATCH"
	CodeMandateKindNotAllowed  = "MANDATE_KIND_NOT_ALLOWED"
	CodeMandateBindingMismatch = "MANDATE_BINDING_MISMATCH"
)

var (
	// ErrMandateRequired is a governed target with no attached mandate.
	ErrMandateRequired = errors.New("runmandate: no mandate is attached")
	// ErrMandateNotFound is an attached id that names no mandate.
	ErrMandateNotFound = errors.New("runmandate: the mandate does not exist")
	// ErrMandateNotUsable is a mandate that is not active, is expired, or
	// whose mandate, binding or delegation epoch is not current.
	ErrMandateNotUsable = errors.New("runmandate: the mandate is not usable")
	// ErrScopeMismatch is a cron or input the mandate's scopes do not cover.
	ErrScopeMismatch = errors.New("runmandate: the mandate's scope does not cover the run")
	// ErrTenantMismatch is a mandate of another tenant project.
	ErrTenantMismatch = errors.New("runmandate: the mandate belongs to another tenant")
	// ErrKindNotAllowed is a mandate kind no unattended run may use: one
	// that is not autonomous or delegated_background, or one the active
	// policy revision does not allow (gate D4).
	ErrKindNotAllowed = errors.New("runmandate: the mandate kind is not allowed for unattended runs")
	// ErrBindingMismatch is a mandate whose binding does not bind the
	// version the target starts.
	ErrBindingMismatch = errors.New("runmandate: the mandate's agent binding does not match the target")
)

// Code is the result code of err; empty for an error of no code here.
func Code(err error) string {
	switch {
	case errors.Is(err, ErrMandateRequired):
		return CodeMandateRequired
	case errors.Is(err, ErrMandateNotFound):
		return CodeMandateNotFound
	case errors.Is(err, ErrMandateNotUsable):
		return CodeMandateNotUsable
	case errors.Is(err, ErrScopeMismatch):
		return CodeMandateScopeMismatch
	case errors.Is(err, ErrTenantMismatch):
		return CodeMandateTenantMismatch
	case errors.Is(err, ErrKindNotAllowed):
		return CodeMandateKindNotAllowed
	case errors.Is(err, ErrBindingMismatch):
		return CodeMandateBindingMismatch
	}
	return ""
}

// Target is what an unattended run starts.
type Target struct {
	Origin          Origin
	TenantProjectID int64
	// ResourceID names the schedule, trigger or index schedule in audit
	// events.
	ResourceID string
	// ApplicationVersionID is the version a pipeline schedule or trigger
	// starts; 0 for an index schedule, whose binding the mandate supplies.
	ApplicationVersionID int64
	// Cron is the schedule's expression; empty for a trigger.
	Cron string
	// Crons are further expressions the target fires on: the attach of an
	// index schedule names every per-user schedule of the index, and the
	// mandate must cover each.
	Crons []string
	// Input is the stored run input; nil for a trigger, whose input the
	// sender writes per call.
	Input *string
}

// BindingReader reads a binding as a governed decision must see it
// (repos.AgentIdentityBindings.GetUsable).
type BindingReader interface {
	GetUsable(ctx context.Context, id string) (agentidentity.AgentIdentityBinding, error)
}

// KindPolicy is the active policy revision's unattended mandate kinds
// (profile.PolicyDocument).
type KindPolicy interface {
	AllowsUnattendedMandateKind(kind string) bool
}

// Refusal is one skipped or refused unattended run.
type Refusal struct {
	Target    Target
	MandateID string
	Code      string
	At        time.Time
}

// RefusalRecorder appends the security audit event of a refusal.
type RefusalRecorder interface {
	RecordRunRefusal(ctx context.Context, refusal Refusal) error
}

// Config is the service's dependencies; all are required.
type Config struct {
	Mandates mandate.Reader
	Bindings BindingReader
	Epochs   epoch.Reader
	Policy   KindPolicy
	Refusals RefusalRecorder
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Service checks attached mandates.
type Service struct {
	mandates mandate.Reader
	bindings BindingReader
	epochs   epoch.Reader
	policy   KindPolicy
	refusals RefusalRecorder
	now      func() time.Time
}

// New builds the service.
func New(cfg Config) (*Service, error) {
	if cfg.Mandates == nil || cfg.Bindings == nil || cfg.Epochs == nil || cfg.Policy == nil || cfg.Refusals == nil {
		return nil, errors.New("runmandate: mandates, bindings, epochs, a kind policy and a refusal recorder are required")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		mandates: cfg.Mandates, bindings: cfg.Bindings, epochs: cfg.Epochs,
		policy: cfg.Policy, refusals: cfg.Refusals, now: now,
	}, nil
}

// CheckAttach answers whether mandateID may be attached to target: same
// tenant, an allowed kind, a usable binding of the target's version, not
// revoked or expired, and scopes that cover the target. A proposed or
// suspended mandate may be attached; it runs nothing until it is active.
func (s *Service) CheckAttach(ctx context.Context, mandateID string, target Target) (mandate.Mandate, error) {
	m, err := s.read(ctx, mandateID, target)
	if err != nil {
		return mandate.Mandate{}, err
	}
	switch m.EffectiveState(s.now().UTC()) {
	case mandate.StateRevoked, mandate.StateExpired:
		return mandate.Mandate{}, ErrMandateNotUsable
	}
	return m, nil
}

// Covers answers whether the attached mandate still covers target, for a
// governed save that changes the cron or the input (ErrScopeMismatch). It
// reads the mandate only: an inactive mandate is the fire's concern.
func (s *Service) Covers(ctx context.Context, mandateID string, target Target) error {
	m, err := s.mandates.Get(ctx, mandateID)
	if errors.Is(err, mandate.ErrMandateNotFound) {
		return ErrMandateNotFound
	}
	if err != nil {
		return fmt.Errorf("runmandate: read the mandate: %w", err)
	}
	if !Covers(m, target) {
		return ErrScopeMismatch
	}
	return nil
}

// Admit is the fire-time check. An empty mandateID is ErrMandateRequired:
// there is no fallback to a person or a system token.
func (s *Service) Admit(ctx context.Context, mandateID string, target Target) (securitycontext.RunAuthority, error) {
	if strings.TrimSpace(mandateID) == "" {
		return securitycontext.RunAuthority{}, ErrMandateRequired
	}
	m, err := s.read(ctx, mandateID, target)
	if err != nil {
		return securitycontext.RunAuthority{}, err
	}
	if m.EffectiveState(s.now().UTC()) != mandate.StateActive || s.now().UTC().Before(m.NotBefore) {
		return securitycontext.RunAuthority{}, ErrMandateNotUsable
	}
	keys := []epoch.SecurityEpoch{
		{Kind: epoch.KindMandate, ID: m.ID, Value: epoch.Unpinned},
		{Kind: epoch.KindBinding, ID: m.AgentBindingID, Value: epoch.Unpinned},
	}
	if m.DelegationID != nil {
		keys = append(keys, epoch.SecurityEpoch{Kind: epoch.KindDelegation, ID: *m.DelegationID, Value: epoch.Unpinned})
	}
	current, err := s.epochs.Read(ctx, keys)
	if err != nil {
		return securitycontext.RunAuthority{}, fmt.Errorf("runmandate: read the epochs: %w", err)
	}
	if len(epoch.Stale(keys, current)) > 0 || current[0].Value != m.RevocationEpoch {
		// A record that is missing or not active, or a mandate that moved
		// between the two reads: refused, and the next fire reads again.
		return securitycontext.RunAuthority{}, ErrMandateNotUsable
	}
	authority := securitycontext.RunAuthority{MandateID: m.ID, MandateEpoch: m.RevocationEpoch}
	if m.SubjectUserID != nil {
		authority.SubjectUserID = *m.SubjectUserID
	}
	return authority, nil
}

// Refuse records the security audit event of a refused run. cause is the
// error Admit answered; one with no result code is not recorded.
func (s *Service) Refuse(ctx context.Context, target Target, mandateID string, cause error) error {
	code := Code(cause)
	if code == "" {
		return nil
	}
	return s.refusals.RecordRunRefusal(context.WithoutCancel(ctx), Refusal{
		Target: target, MandateID: mandateID, Code: code, At: s.now().UTC(),
	})
}

// read is the checks attach and fire share.
func (s *Service) read(ctx context.Context, mandateID string, target Target) (mandate.Mandate, error) {
	if strings.TrimSpace(mandateID) == "" {
		return mandate.Mandate{}, ErrMandateRequired
	}
	m, err := s.mandates.Get(ctx, mandateID)
	if errors.Is(err, mandate.ErrMandateNotFound) {
		return mandate.Mandate{}, ErrMandateNotFound
	}
	if err != nil {
		return mandate.Mandate{}, fmt.Errorf("runmandate: read the mandate: %w", err)
	}
	if m.TenantProjectID != target.TenantProjectID {
		return mandate.Mandate{}, ErrTenantMismatch
	}
	if !m.Kind.Valid() || !s.policy.AllowsUnattendedMandateKind(string(m.Kind)) {
		return mandate.Mandate{}, ErrKindNotAllowed
	}
	binding, err := s.bindings.GetUsable(ctx, m.AgentBindingID)
	if errors.Is(err, agentidentity.ErrBindingNotFound) {
		return mandate.Mandate{}, ErrBindingMismatch
	}
	if err != nil {
		return mandate.Mandate{}, fmt.Errorf("runmandate: read the binding: %w", err)
	}
	if binding.TenantProjectID != m.TenantProjectID ||
		(target.ApplicationVersionID > 0 && binding.ApplicationVersionID != target.ApplicationVersionID) {
		return mandate.Mandate{}, ErrBindingMismatch
	}
	if binding.State != agentidentity.BindingActive {
		return mandate.Mandate{}, ErrMandateNotUsable
	}
	if !Covers(m, target) {
		return mandate.Mandate{}, ErrScopeMismatch
	}
	return m, nil
}

// The resource scope prefixes this package reads.
const (
	ScopeCronPrefix  = "schedule.cron:"
	ScopeInputPrefix = "schedule.input:sha256:"
)

// CronScope is the resource scope that names expression.
func CronScope(expression string) string {
	return ScopeCronPrefix + strings.Join(strings.Fields(expression), "_")
}

// InputScope is the resource scope that names input.
func InputScope(input string) string {
	digest := sha256.Sum256([]byte(input))
	return ScopeInputPrefix + hex.EncodeToString(digest[:])
}

// Covers reports whether m's resource scopes cover target's cron and input.
// It fails closed: a mandate restricted to crons covers no target without a
// cron (a trigger, an index with no schedule), and one restricted to inputs
// covers no target without a stored input (a trigger, whose sender writes
// the input per call, and an index schedule).
func Covers(m mandate.Mandate, target Target) bool {
	var crons, inputs []string
	for _, scope := range m.ResourceScopes {
		switch {
		case strings.HasPrefix(scope, ScopeCronPrefix):
			crons = append(crons, scope)
		case strings.HasPrefix(scope, ScopeInputPrefix):
			inputs = append(inputs, strings.ToLower(scope))
		}
	}
	if len(crons) > 0 {
		expressions := target.Crons
		if target.Cron != "" {
			expressions = append([]string{target.Cron}, expressions...)
		}
		if len(expressions) == 0 {
			return false
		}
		for _, expression := range expressions {
			if !contains(crons, CronScope(expression)) {
				return false
			}
		}
	}
	if len(inputs) > 0 && (target.Input == nil || !contains(inputs, InputScope(*target.Input))) {
		return false
	}
	return true
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
