package repos

// The operator path for an OIDC subject that changed (issue #1460, item 12).
//
// An identity provider that deletes and creates a user again gives the new
// user a new `sub`. The account here still holds the old link
// (`auth_core__user_provider.provider_ref = 'oidc:<old sub>'`), so the first
// login of the new subject falls through to the email match, and the email
// match refuses an account that another OIDC subject already holds
// (identityrepo.AdoptionGuard). The login then answers 409. That refusal is
// correct: only an operator can say that the two subjects are one person.
//
// RelinkOIDCIdentityTx is that operator decision. It either replaces the old
// subject with the new one, or removes the old link so that the next login
// adopts the account by its email (with the usual email rules: a verified
// address where the deployment requires one, and no SCIM account unless the
// provider adopts SCIM users). Every change writes one security audit event in
// the same transaction; an append failure rolls the change back.
//
// It refuses every ambiguous case rather than choosing:
//   - an email that names more than one account (compared without case);
//   - an account with more than one OIDC link, unless the caller names the
//     link to change;
//   - a new subject that another account already holds.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identityrepo"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// Audit actions of RelinkOIDCIdentityTx.
const (
	ActionOIDCLinkReplaced = "identity.oidc_link.replaced"
	ActionOIDCLinkCleared  = "identity.oidc_link.cleared"
	// ActionOIDCLinkAdded is a new subject on an account with no OIDC link.
	ActionOIDCLinkAdded = "identity.oidc_link.added"
)

// Refusals of RelinkOIDCIdentityTx. The caller prints them; none carries a
// database error text.
var (
	ErrRelinkUserNotFound     = errors.New("no account matches the user")
	ErrRelinkAmbiguousUser    = errors.New("the email matches more than one account; give the numeric user id")
	ErrRelinkAmbiguousLink    = errors.New("the account has more than one OIDC link; name the one to change with the old subject")
	ErrRelinkOldLinkNotFound  = errors.New("the account does not hold the named old subject")
	ErrRelinkSubjectTaken     = errors.New("another account already holds the new subject")
	ErrRelinkInvalidSubject   = errors.New("the subject is empty, too long, or holds control characters")
	ErrRelinkInvalidOperation = errors.New("give exactly one of a new subject or clear")
)

// OIDCRelinkRequest is one relink.
type OIDCRelinkRequest struct {
	// User is the account: a numeric auth_core user id, or an email address.
	User string
	// NewSubject is the new OIDC `sub`, without the "oidc:" prefix. Empty
	// together with Clear.
	NewSubject string
	// Clear removes the OIDC link instead of replacing it.
	Clear bool
	// OldSubject names the link to change when the account has more than
	// one. Empty selects the only one.
	OldSubject string
	// Operator is the auth_core user id of the operator, recorded as the
	// audit principal.
	Operator string
	// Apply writes the change. False only plans it.
	Apply bool
}

// OIDCRelinkPlan is what a relink changes, or would change.
type OIDCRelinkPlan struct {
	UserID    int
	Email     string
	Suspended bool
	// Links are the account's OIDC subjects before the change.
	Links []string
	// Remove is the subject the change removes, "" when none.
	Remove string
	// Add is the subject the change adds, "" when none.
	Add string
	// NoChange reports that the account is already in the requested state.
	NoChange bool
	// Applied reports that the change was written.
	Applied bool
	// AuditEventID is the security audit event of an applied change.
	AuditEventID string
}

const maxOIDCSubjectBytes = 255

func validOIDCSubject(subject string) bool {
	if subject == "" || len(subject) > maxOIDCSubjectBytes {
		return false
	}
	for _, r := range subject {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// subjectDigest is the audit form of a subject: "sha256:<hex>".
func subjectDigest(subject string) string {
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:])
}

// RelinkOIDCIdentityTx plans, and with Apply writes, one relink in tx.
func RelinkOIDCIdentityTx(ctx context.Context, tx pgx.Tx, req OIDCRelinkRequest) (OIDCRelinkPlan, error) {
	req.User = strings.TrimSpace(req.User)
	if req.Clear == (req.NewSubject != "") {
		return OIDCRelinkPlan{}, ErrRelinkInvalidOperation
	}
	if req.NewSubject != "" && !validOIDCSubject(req.NewSubject) {
		return OIDCRelinkPlan{}, ErrRelinkInvalidSubject
	}
	if req.OldSubject != "" && !validOIDCSubject(req.OldSubject) {
		return OIDCRelinkPlan{}, ErrRelinkInvalidSubject
	}

	plan, err := relinkResolveUser(ctx, tx, req.User)
	if err != nil {
		return OIDCRelinkPlan{}, err
	}
	rows, err := tx.Query(ctx, `
SELECT provider_ref FROM public.auth_core__user_provider
WHERE user_id = $1 AND provider_ref LIKE $2
ORDER BY provider_ref
FOR UPDATE`, plan.UserID, identityrepo.OIDCProviderRefPrefix+"%")
	if err != nil {
		return OIDCRelinkPlan{}, fmt.Errorf("oidc relink: read the links: %w", err)
	}
	refs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return OIDCRelinkPlan{}, fmt.Errorf("oidc relink: read the links: %w", err)
	}
	for _, ref := range refs {
		plan.Links = append(plan.Links, strings.TrimPrefix(ref, identityrepo.OIDCProviderRefPrefix))
	}

	switch {
	case req.OldSubject != "":
		found := false
		for _, link := range plan.Links {
			found = found || link == req.OldSubject
		}
		if !found {
			return plan, ErrRelinkOldLinkNotFound
		}
		plan.Remove = req.OldSubject
	case len(plan.Links) == 1:
		plan.Remove = plan.Links[0]
	case len(plan.Links) > 1:
		// Two providers, or a link added by hand: the operator names it.
		if req.NewSubject == "" || !slices.Contains(plan.Links, req.NewSubject) {
			return plan, ErrRelinkAmbiguousLink
		}
	}

	if req.NewSubject != "" {
		if slices.Contains(plan.Links, req.NewSubject) {
			plan.Remove, plan.NoChange = "", true
			return plan, nil
		}
		var holder int
		err := tx.QueryRow(ctx, `
SELECT user_id FROM public.auth_core__user_provider WHERE provider_ref = $1`,
			identityrepo.OIDCProviderRefPrefix+req.NewSubject).Scan(&holder)
		switch {
		case err == nil:
			return plan, fmt.Errorf("%w (user id %d)", ErrRelinkSubjectTaken, holder)
		case !errors.Is(err, pgx.ErrNoRows):
			return plan, fmt.Errorf("oidc relink: check the new subject: %w", err)
		}
		plan.Add = req.NewSubject
	} else if plan.Remove == "" {
		plan.NoChange = true
		return plan, nil
	}

	if !req.Apply {
		return plan, nil
	}
	operatorID, operator, err := CanonicalUserID(req.Operator)
	if err != nil || operatorID <= 0 {
		return plan, errors.New("oidc relink: the operator must be a numeric auth_core user id")
	}

	if plan.Remove != "" {
		if _, err := tx.Exec(ctx, `
DELETE FROM public.auth_core__user_provider WHERE user_id = $1 AND provider_ref = $2`,
			plan.UserID, identityrepo.OIDCProviderRefPrefix+plan.Remove); err != nil {
			return plan, fmt.Errorf("oidc relink: remove the old link: %w", err)
		}
	}
	if plan.Add != "" {
		if _, err := tx.Exec(ctx, `
INSERT INTO public.auth_core__user_provider (user_id, provider_ref) VALUES ($1, $2)`,
			plan.UserID, identityrepo.OIDCProviderRefPrefix+plan.Add); err != nil {
			return plan, fmt.Errorf("oidc relink: add the new link: %w", err)
		}
	}

	action := ActionOIDCLinkReplaced
	switch {
	case plan.Add == "":
		action = ActionOIDCLinkCleared
	case plan.Remove == "":
		action = ActionOIDCLinkAdded
	}
	reasons := []string{}
	if plan.Remove != "" {
		reasons = append(reasons, "old_subject_sha256:"+subjectDigest(plan.Remove))
	}
	if plan.Add != "" {
		reasons = append(reasons, "new_subject_sha256:"+subjectDigest(plan.Add))
	}
	appender, err := securityaudit.NewPostgresAppender("elitea-main/oidc-relink")
	if err != nil {
		return plan, fmt.Errorf("oidc relink: the audit appender: %w", err)
	}
	record, err := appender.AppendRecord(ctx, tx, securityaudit.SecurityEvent{
		Type:         securityaudit.TypeIdentityLinkChanged,
		Action:       action,
		Effect:       securityaudit.EffectAllow,
		Reasons:      reasons,
		PrincipalRef: "user:" + operator,
		ResourceType: "user",
		ResourceID:   strconv.Itoa(plan.UserID),
	})
	if err != nil {
		return plan, fmt.Errorf("oidc relink: append the audit event: %w", err)
	}
	plan.Applied, plan.AuditEventID = true, record.EventID
	return plan, nil
}

// relinkResolveUser finds the one account that user names.
func relinkResolveUser(ctx context.Context, tx pgx.Tx, user string) (OIDCRelinkPlan, error) {
	if user == "" {
		return OIDCRelinkPlan{}, ErrRelinkUserNotFound
	}
	query := `
SELECT id, coalesce(email, ''), suspended FROM public.auth_core__user
WHERE lower(email) = lower($1)
ORDER BY id
LIMIT 2
FOR UPDATE`
	var arg any = user
	if id, err := strconv.Atoi(user); err == nil {
		query = `
SELECT id, coalesce(email, ''), suspended FROM public.auth_core__user
WHERE id = $1
FOR UPDATE`
		arg = id
	}
	rows, err := tx.Query(ctx, query, arg)
	if err != nil {
		return OIDCRelinkPlan{}, fmt.Errorf("oidc relink: read the account: %w", err)
	}
	plans, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (OIDCRelinkPlan, error) {
		var plan OIDCRelinkPlan
		err := row.Scan(&plan.UserID, &plan.Email, &plan.Suspended)
		return plan, err
	})
	if err != nil {
		return OIDCRelinkPlan{}, fmt.Errorf("oidc relink: read the account: %w", err)
	}
	switch len(plans) {
	case 0:
		return OIDCRelinkPlan{}, ErrRelinkUserNotFound
	case 1:
		return plans[0], nil
	default:
		return OIDCRelinkPlan{}, ErrRelinkAmbiguousUser
	}
}
