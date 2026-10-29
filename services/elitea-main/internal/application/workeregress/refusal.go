package workeregress

import (
	"errors"
	"fmt"
	"strings"
)

// RefusalCode is the code a refused policy settles its run with: the
// `code` of the run's `execution.failed` event and of the stored refusal
// (elitea_runtime.command_outbox.refusal_code, shared 0197).
const RefusalCode = "EGRESS_POLICY_INVALID"

// ChatErrorCode is the same refusal as a chat answer's `error_code`, in the
// chat meta's own lower-case vocabulary.
const ChatErrorCode = "egress_policy_invalid"

// MaxRefusalMessageBytes bounds a refusal's message (the column's CHECK).
const MaxRefusalMessageBytes = 1024

// RefusalError is a policy that can never be assembled for this command as
// it stands: a malformed platform or project entry, a list over the bounds,
// or an input the command names that is missing or unreadable. Retrying
// cannot change the answer, so the run fails at once with Message, which
// says what is wrong and who fixes it. It names an administrator's entry,
// never a credential, a secret or a toolkit setting's value.
//
// Every other error of the resolver is transient (a source could not be
// read): the dispatch is retried, bounded by the run's deadline.
type RefusalError struct {
	Message string
}

func (e *RefusalError) Error() string { return "worker egress policy refused: " + e.Message }

// DispatchRefusal answers the code and message the run fails with
// (commandbus reads it without importing this package).
func (e *RefusalError) DispatchRefusal() (string, string) { return RefusalCode, e.Message }

// Is makes a refusal match ErrPolicyRefused.
func (e *RefusalError) Is(target error) bool { return target == ErrPolicyRefused }

// AsRefusal answers the refusal err carries, if any.
func AsRefusal(err error) (*RefusalError, bool) {
	var refusal *RefusalError
	if errors.As(err, &refusal) && refusal != nil {
		return refusal, true
	}
	return nil, false
}

func refuse(format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	if len(message) > MaxRefusalMessageBytes {
		message = strings.ToValidUTF8(message[:MaxRefusalMessageBytes-3], "") + "..."
	}
	return &RefusalError{Message: message}
}

// quoteEntry quotes an administrator's entry for a message, cut short so a
// pasted blob cannot fill it.
func quoteEntry(entry string) string {
	const max = 96
	if len(entry) > max {
		entry = strings.ToValidUTF8(entry[:max], "") + "..."
	}
	return fmt.Sprintf("%q", entry)
}

const (
	platformFixer = "A platform administrator must correct it under Configuration, Worker Egress."
	projectFixer  = "A project administrator must correct it in the project's Worker Egress setting."
	boundsFixer   = "A platform or project administrator must remove worker egress allowlist entries, " +
		"or the agent must use fewer toolkits and MCP servers."
)
