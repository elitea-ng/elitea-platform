package execution

import "errors"

// ErrDispatchRefused matches every *DispatchRefusal.
var ErrDispatchRefused = errors.New("dispatch refused")

// ErrDispatchNotRefused is a refusal that was NOT recorded: the run's outbox
// row changed under the dispatch (another replica prepared it, it was
// claimed or retired, or the run left PENDING/DISPATCHED). It is an ordinary
// dispatch error, never a *DispatchRefusal: no caller may report the run as
// failed with the refusal, and the next dispatch pass acts on the row's
// actual state (publishes a prepared envelope, or skips a retired row).
var ErrDispatchNotRefused = errors.New("dispatch refusal not recorded: the run's outbox row is no longer refusable")

// DispatchRefusal is a command Main refused to prepare because it can never
// succeed as it stands (today: a worker egress allowlist that cannot be
// assembled, ADR-0044 decision 2). By the time a dispatcher sees it, the
// refusal is durable on the run's outbox row (shared 0197) and the row's
// deadline has passed, so the no-authority deadline retirement settles the
// run with Code and Message as its `execution.failed` event. A dispatcher
// that waits for the run (a tool call, a tool discovery) answers it to its
// caller at once; the outbox publisher counts it as handled, not as a
// failure to retry.
type DispatchRefusal struct {
	Code    string
	Message string
}

func (r *DispatchRefusal) Error() string { return "dispatch refused: " + r.Code + ": " + r.Message }

// Is makes a refusal match ErrDispatchRefused.
func (r *DispatchRefusal) Is(target error) bool { return target == ErrDispatchRefused }

// AsDispatchRefusal answers the refusal err carries, if any.
func AsDispatchRefusal(err error) (*DispatchRefusal, bool) {
	var refusal *DispatchRefusal
	if errors.As(err, &refusal) && refusal != nil {
		return refusal, true
	}
	return nil, false
}
