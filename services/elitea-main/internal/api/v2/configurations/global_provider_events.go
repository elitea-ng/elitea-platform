package configurations

// The admin.access.changed security audit events of the global provider
// routes (ADR-0059 decision 6 and decision 8, story R1.5, issue 2509). A
// global provider is a platform credential every project can route through,
// so creating one, deleting one or changing its endpoint, credential or
// keyless identity is a change of who can reach what: kind
// global_provider_route. The event names the data fields the write carried,
// never their values or a digest of them, and is appended in the write's
// transaction through securityaudit.RunChangeHook, so a failed append rolls
// the write back.

import (
	"context"
	"net/http"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit/accessevent"
)

const globalProviderEventSource = "elitea-main/configurations"

// WithAccessAuditAppender replaces the appender of the global provider
// routes' events. Production keeps the PostgreSQL appender; a test passes a
// failing one to show that the write rolls back.
func WithAccessAuditAppender(appender securityaudit.Appender) Option {
	return func(handler *Handler) {
		if appender != nil {
			handler.accessAudit = appender
		}
	}
}

func (h *Handler) accessAppender() securityaudit.Appender {
	if h.accessAudit != nil {
		return h.accessAudit
	}
	return accessevent.DefaultAppender(globalProviderEventSource)
}

// globalProviderFieldName is a data key the event names as it is: a plain
// identifier, which the event's string rules always admit (no dot, so no
// credential-shaped value).
var globalProviderFieldName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// maxGlobalProviderFieldNames bounds the data keys an event names, well
// under the event's set cap.
const maxGlobalProviderFieldNames = 128

// globalProviderOtherField stands for every data key not named.
const globalProviderOtherField = "data.<other>"

// The operations a global provider event names in Fields beside the data
// fields.
const (
	globalProviderCreated = "created"
	globalProviderDeleted = "deleted"
)

// globalProviderFields names what a write changes: the operation for a create
// or a delete, the keys of the body's `data` object (the endpoint, the
// credential, the keyless identity and the rest of the route) and its type.
// Empty when an update changes nothing of the route (a label alone), which
// appends no event.
func globalProviderFields(body map[string]any, operation string) []string {
	var fields []string
	if operation != "" {
		fields = append(fields, operation)
	}
	if data, ok := body["data"].(map[string]any); ok {
		named := 0
		for key := range data {
			if !globalProviderFieldName.MatchString(key) || named >= maxGlobalProviderFieldNames {
				// A key the event cannot carry by name (too long, unusual
				// characters, past the cap) is still a change: it is
				// recorded as one other field, never refused.
				fields = append(fields, globalProviderOtherField)
				continue
			}
			named++
			fields = append(fields, "data."+key)
		}
	}
	if _, ok := body["type"]; ok {
		fields = append(fields, "type")
	}
	slices.Sort(fields)
	return slices.Compact(fields)
}

// withGlobalProviderEvent arms the change hook of one global provider write
// on the request: the write runs it in its transaction with the row id. No
// fields, no hook.
func (h *Handler) withGlobalProviderEvent(r *http.Request, fields []string) *http.Request {
	if len(fields) == 0 {
		return r
	}
	appender := h.accessAppender()
	requestContext := r.Context()
	hook := func(ctx context.Context, tx pgx.Tx, subjectID string) error {
		change := accessevent.FromRequest(requestContext, securityaudit.AdminAccessChanged{
			SubjectType: "global_provider",
			SubjectID:   subjectID,
			Change: securityaudit.AccessChange{
				Kind:   securityaudit.AccessGlobalProviderRoute,
				Fields: fields,
			},
		})
		return appender.Append(ctx, tx, securityaudit.NewAdminAccessChanged(change))
	}
	return r.WithContext(securityaudit.ContextWithChangeHook(requestContext, hook))
}
