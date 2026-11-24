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
func RunChangeHook(ctx context.Context, tx pgx.Tx, subjectID string) error {
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
