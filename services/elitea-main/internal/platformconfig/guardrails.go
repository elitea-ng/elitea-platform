package platformconfig

import (
	"context"
	"errors"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/guardrails"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LoadGuardrails resolves the `guardrails` section into a matching policy.
//
// It returns the error rather than swallowing it, and that is a deliberate
// departure from the flag readers above.
//
// Those readers degrade permissively on their own, because a database hiccup
// must not silently switch a subsystem off. Here the permissive answer is the
// UNSAFE one: an empty policy blocks nothing, so a failed read would hand back
// exactly the tools an operator disabled. The two callers want opposite things
// from that, and neither can be served by a single hidden default:
//
//   - the catalogue and form endpoints degrade permissively and say so in the
//     log. Refusing to list toolkit types because one row could not be read
//     would take the product down to enforce a policy that may well be empty.
//   - the agent tool FREEZE refuses the execution. That is the one path where
//     running unguarded means a blocked tool actually executes, and "we could
//     not read the policy" is not a reason to act as though there were none.
//
// Deciding that here would have made one of those two wrong. Callers decide.
func LoadGuardrails(ctx context.Context, pool *pgxpool.Pool) (guardrails.Policy, error) {
	values, err := Load(ctx, pool, SectionGuardrails)
	if err != nil {
		return guardrails.Policy{}, err
	}
	suspended, err := loadSuspendedToolkitTypes(ctx, pool)
	if err != nil {
		return guardrails.Policy{}, err
	}
	return guardrails.NewPolicy(guardrails.PolicyInput{
		BlockedToolkits: values.Strings(KeyBlockedToolkits),
		BlockedTools:    values.StringLists(KeyBlockedTools),
		SensitiveTools:  values.StringLists(KeySensitiveTools),
		CompanyName:     values.String(KeySensitiveActionCompanyName, ""),
		MessageTemplate: values.String(KeySensitiveActionMessageTemplate, ""),
	}).WithSuspendedToolkits(suspended), nil
}

// loadSuspendedToolkitTypes reads the toolkit types under an active
// toolkit_type suspension (shared 0194, ADR-0041 decision 1). The rows hold
// canonical keys already. Read with the rest of the policy, uncached, for
// the same reason the policy is: a suspension must reach the next frozen
// run, not the next deployment. The partial unique index keeps the scan to
// the active rows.
func loadSuspendedToolkitTypes(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	if pool == nil { // no store, as in Load: nothing is suspended
		return nil, nil
	}
	rows, err := pool.Query(ctx, `
SELECT scope_key FROM elitea_runtime.execution_suspensions
WHERE scope = 'toolkit_type' AND resumed_at IS NULL AND scope_key IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("read suspended toolkit types: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("read suspended toolkit types: %w", err)
		}
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read suspended toolkit types: %w", err)
	}
	return out, nil
}

// GuardrailPolicyAdapter is the single implementation of the policy source that
// three packages declare interfaces for — the toolkit API surfaces, the agent
// tool freeze, and the admin configuration form.
//
// One adapter satisfies all of them because there is exactly one answer. It
// lives here rather than in internal/runtimecomposition so that internal/api can
// construct it directly from the pool it already holds: the composition root
// imports the api packages, so an adapter owned there could only reach them by
// inverting that edge or by threading another RouterConfig field through main.
//
// No cache. A save must take effect on the next call, not on the next
// deployment — see this package's header on why the reference needed
// `requires_restart` on every field and this service does not.
type GuardrailPolicyAdapter struct {
	pool *pgxpool.Pool
}

// NewGuardrailPolicyAdapter refuses a nil pool rather than returning an adapter
// that would answer "nothing is blocked" forever.
func NewGuardrailPolicyAdapter(pool *pgxpool.Pool) (*GuardrailPolicyAdapter, error) {
	if pool == nil {
		return nil, errors.New("guardrail policy adapter requires a database pool")
	}
	return &GuardrailPolicyAdapter{pool: pool}, nil
}

// GuardrailPolicy satisfies the toolkit API surfaces' source, which degrade
// permissively on error.
func (adapter *GuardrailPolicyAdapter) GuardrailPolicy(
	ctx context.Context,
) (guardrails.Policy, error) {
	if adapter == nil || adapter.pool == nil {
		return guardrails.Policy{}, errors.New("guardrail policy adapter is not configured")
	}
	return LoadGuardrails(ctx, adapter.pool)
}

// ResolveCurrentAgentGuardrails satisfies the agent freeze's source, which fails
// the execution on error. The same read, handled oppositely by its two callers —
// which is why LoadGuardrails returns the error rather than choosing for them.
func (adapter *GuardrailPolicyAdapter) ResolveCurrentAgentGuardrails(
	ctx context.Context,
) (guardrails.Policy, error) {
	return adapter.GuardrailPolicy(ctx)
}
