package main

// `elitea-main break-glass issue | list | revoke`: the sealed credentials
// (ADR-0059 decision 5, "Path 2"; story R2.6a, issue 2520). The store is
// internal/adminaccess/sealed.go; the group and the exit codes are
// break_glass.go's.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// breakGlassIssueTx issues one credential and answers the lines that show
// it. The lines are printed after the commit, and only there: the secret is
// in no log line, no event and no row.
func breakGlassIssueTx(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, req breakGlassRequest, at time.Time) ([]string, error) {
	person, err := resolveAdminRoleUser(ctx, tx, req.user)
	if err != nil {
		return nil, err
	}
	if person.suspended {
		return nil, refusedError("%s is suspended; a sealed credential is bound to an active account", person.label())
	}
	issued, err := adminaccess.IssueSealedCredential(ctx, tx, appender, adminaccess.SealedIssue{
		UserID: person.id, Level: req.level, Label: req.label, Split: req.split, At: at, Lifetime: req.lifetime,
		Source: securityaudit.ElevationSourceOperator,
	})
	switch {
	case errors.Is(err, adminaccess.ErrSealedCredentialPerson):
		return nil, refusedError("%s is not an active account", person.label())
	case err != nil:
		return nil, err
	}
	lines := []string{
		fmt.Sprintf("issued sealed credential %s for %s at the level %s, expires %s%s",
			issued.ID, person.label(), issued.Level, issued.ExpiresAt.UTC().Format(time.RFC3339), labelSuffix(issued.Label)),
		"It is shown this once and stored only as a digest: seal it now. Revoke it with `" +
			breakGlassCommand + " " + breakGlassRevoke + " " + issued.ID.String() + "`.",
	}
	if !issued.Split {
		return append(lines, issued.Credential()), nil
	}
	first, second := issued.Halves()
	return append(lines,
		"Split knowledge: give each half to a different custodian. The sign-in page asks for the selector and both halves.",
		"selector: "+adminaccess.SealedCredentialPrefix+issued.Selector,
		"half 1: "+first,
		"half 2: "+second,
	), nil
}

func labelSuffix(label string) string {
	if label == "" {
		return ""
	}
	return fmt.Sprintf(", label %q", label)
}

// breakGlassListTx lists every credential and records the read.
func breakGlassListTx(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, at time.Time) ([]string, error) {
	credentials, err := adminaccess.ListSealedCredentials(ctx, tx, appender, adminaccess.SealedList{
		Source: securityaudit.ElevationSourceOperator,
	})
	if err != nil {
		return nil, err
	}
	if len(credentials) == 0 {
		return []string{"no sealed credential"}, nil
	}
	ids := make([]int64, 0, len(credentials))
	for _, credential := range credentials {
		ids = append(ids, credential.UserID)
	}
	emails := map[int64]string{}
	rows, err := tx.Query(ctx, `SELECT id, COALESCE(email, '') FROM public.auth_core__user WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("read the people of the credentials: %w", err)
	}
	var id int64
	var email string
	if _, err := pgx.ForEachRow(rows, []any{&id, &email}, func() error { emails[id] = email; return nil }); err != nil {
		return nil, fmt.Errorf("read the people of the credentials: %w", err)
	}
	lines := make([]string, 0, len(credentials))
	for _, c := range credentials {
		line := fmt.Sprintf("%s %s level=%s user=%d <%s> split=%t issued=%s expires=%s",
			c.ID, c.State(at), c.Level, c.UserID, emails[c.UserID], c.Split,
			c.IssuedAt.UTC().Format(time.RFC3339), c.ExpiresAt.UTC().Format(time.RFC3339))
		if !c.UsedAt.IsZero() {
			line += " used=" + c.UsedAt.UTC().Format(time.RFC3339)
		}
		if !c.RevokedAt.IsZero() {
			line += " revoked=" + c.RevokedAt.UTC().Format(time.RFC3339)
		}
		if c.Label != "" {
			line += fmt.Sprintf(" label=%q", c.Label)
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// breakGlassRevokeTx revokes every id, or refuses the whole command.
func breakGlassRevokeTx(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, ids []uuid.UUID, at time.Time) ([]string, error) {
	lines := make([]string, 0, len(ids))
	for _, id := range ids {
		revoked, changed, err := adminaccess.RevokeSealedCredential(ctx, tx, appender, adminaccess.SealedRevoke{
			ID: id, At: at, Source: securityaudit.ElevationSourceOperator,
		})
		switch {
		case errors.Is(err, adminaccess.ErrSealedCredentialNotFound):
			return nil, refusedError("no sealed credential %s", id)
		case err != nil:
			return nil, err
		case !changed:
			return nil, refusedError("sealed credential %s is already %s", id, strings.ToLower(revoked.State(at)))
		}
		lines = append(lines, fmt.Sprintf("revoked sealed credential %s of user %d at %s",
			revoked.ID, revoked.UserID, revoked.RevokedAt.UTC().Format(time.RFC3339)))
	}
	return lines, nil
}
