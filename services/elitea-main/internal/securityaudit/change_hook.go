package securityaudit

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// A change hook lets a route append the security audit event of a change
// whose transaction a store owns (the identity provider store, the SCIM
// client store, the configuration writes). The route puts the hook on the
// request context; the store runs it inside its transaction, after the write
// and before the commit, so the event commits with the change and a failed
// append rolls the change back. The store names the subject it wrote (a new
// row's id is known only there).
//
// A store that does not run the hook leaves the change without its event.
// The route can tell: ChangeHookRan reports false, and the route logs it as
// an error. The DB integration tests of each route assert the event.

// ChangeHook appends the event of one change in tx. subjectID is the id of
// the row the store wrote.
type ChangeHook func(ctx context.Context, tx pgx.Tx, subjectID string) error

type changeHookKey struct{}

type changeHookState struct {
	hook ChangeHook
	ran  bool
}

// ContextWithChangeHook returns ctx carrying hook for the store write the
// request makes next.
func ContextWithChangeHook(ctx context.Context, hook ChangeHook) context.Context {
	return context.WithValue(ctx, changeHookKey{}, &changeHookState{hook: hook})
}

// RunChangeHook runs the hook ctx carries, in tx, and is a no-op without one.
// A store calls it after its write and before its commit, and returns its
// error so the transaction rolls back.
//
// It also runs the transaction hooks armed on ctx (RunTransactionHooks), so
// every store that runs the change hook carries them with no change of its
// own.
func RunChangeHook(ctx context.Context, tx pgx.Tx, subjectID string) error {
	if err := RunTransactionHooks(ctx, tx); err != nil {
		return err
	}
	state, _ := ctx.Value(changeHookKey{}).(*changeHookState)
	if state == nil || state.hook == nil {
		return nil
	}
	if err := state.hook(ctx, tx, subjectID); err != nil {
		return err
	}
	state.ran = true
	return nil
}

// ChangeHookRan reports whether a store ran the hook of ctx successfully.
func ChangeHookRan(ctx context.Context) bool {
	state, _ := ctx.Value(changeHookKey{}).(*changeHookState)
	return state != nil && state.ran
}

// Transaction hooks are writes that must commit with whatever change the
// request makes next, armed BEFORE the handler knows of them: the
// announcement of a `critical` change made under an elevation (ADR-0059
// R2.8b) is one. A middleware installs an empty set on the request context
// (ContextWithTransactionHooks); a later step of the same request arms one
// (ArmTransactionHook); the store that owns the change's transaction runs it
// through RunChangeHook, and a handler that owns its transaction runs it with
// RunTransactionHooks, before its commit. Each armed hook runs once.

type transactionHooksKey struct{}

// TransactionHooks is the set a request carries.
type TransactionHooks struct {
	hooks []func(context.Context, pgx.Tx) error
}

// ContextWithTransactionHooks returns ctx carrying an empty set, unless ctx
// carries one already.
func ContextWithTransactionHooks(ctx context.Context) context.Context {
	if _, ok := ctx.Value(transactionHooksKey{}).(*TransactionHooks); ok {
		return ctx
	}
	return context.WithValue(ctx, transactionHooksKey{}, &TransactionHooks{})
}

// ArmTransactionHook adds hook to the set ctx carries. It reports false when
// ctx carries none: the caller then has nowhere to put the write and must
// refuse the change.
func ArmTransactionHook(ctx context.Context, hook func(context.Context, pgx.Tx) error) bool {
	set, ok := ctx.Value(transactionHooksKey{}).(*TransactionHooks)
	if !ok || hook == nil {
		return false
	}
	set.hooks = append(set.hooks, hook)
	return true
}

// RunTransactionHooks runs every armed hook of ctx that has not run, in tx,
// and is a no-op without one. A failure is returned so the caller rolls the
// change back. A hook runs once.
func RunTransactionHooks(ctx context.Context, tx pgx.Tx) error {
	set, ok := ctx.Value(transactionHooksKey{}).(*TransactionHooks)
	if !ok {
		return nil
	}
	hooks := set.hooks
	set.hooks = nil
	for _, hook := range hooks {
		if err := hook(ctx, tx); err != nil {
			return err
		}
	}
	return nil
}

// TransactionHooksPending reports whether ctx carries an armed hook that no
// transaction ran.
func TransactionHooksPending(ctx context.Context) bool {
	set, ok := ctx.Value(transactionHooksKey{}).(*TransactionHooks)
	return ok && len(set.hooks) > 0
}
