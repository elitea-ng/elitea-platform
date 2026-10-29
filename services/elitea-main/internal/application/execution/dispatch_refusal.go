package execution

import "errors"

// ErrDispatchRefused matches every *DispatchRefusal.
var ErrDispatchRefused = errors.New("dispatch refused")

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
