package main

// `elitea-main oidc-relink`: change the OIDC subject an account is linked to
// (issue #1460, item 12).
//
// An identity provider that deletes and creates a user again gives the user a
// new `sub`. The login is then refused with 409 "this identity is already
// linked to another account", because the account still holds the old
// subject. Before this command, the only remedy was an edit of
// auth_core__user_provider.provider_ref by hand. The command ships in the
// elitea-main image, so it runs where the service runs (kubectl exec, or a
// one-off Job from the same image and Secret). It reads DATABASE_URL.
//
// Usage:
//
//	elitea-main oidc-relink --user USER (--sub NEW_SUB | --clear) [--old-sub OLD_SUB]
//	elitea-main oidc-relink --user USER (--sub NEW_SUB | --clear) [--old-sub OLD_SUB] --apply --operator OPERATOR_ID
//
// USER is the numeric user id or the email address. Without --apply the
// command prints the plan and writes nothing. --sub replaces the old subject
// with the new one. --clear removes the old link; the next login of the user
// then joins the account by email, with the usual email rules. Every applied
// change writes one security audit event
// (ai.elitea.security.v1.identity.link.changed) in the same transaction.
// repos.RelinkOIDCIdentityTx holds the rules and the refusals.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
)

const (
	oidcRelinkCommand = "oidc-relink"
	oidcRelinkTimeout = 2 * time.Minute
)

type oidcRelinkConnect func(ctx context.Context, dsn string) (*pgxpool.Pool, error)

func runOIDCRelink(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer) int {
	return runOIDCRelinkWith(ctx, args, lookup, stdout, stderr, pgxpool.New)
}

// parseOIDCRelinkArgs validates the flags. It returns ok=false after it has
// printed the reason.
func parseOIDCRelinkArgs(args []string, stderr io.Writer) (repos.OIDCRelinkRequest, bool) {
	flags := flag.NewFlagSet(oidcRelinkCommand, flag.ContinueOnError)
	flags.SetOutput(stderr)
	user := flags.String("user", "", "the account: numeric user id or email address")
	sub := flags.String("sub", "", "the new OIDC subject (the id_token `sub`, without \"oidc:\")")
	clearLink := flags.Bool("clear", false, "remove the OIDC link; the next login links the account again by email")
	oldSub := flags.String("old-sub", "", "the subject to change, when the account has more than one OIDC link")
	apply := flags.Bool("apply", false, "write the change (default: print the plan only)")
	operator := flags.String("operator", "", "with --apply: the numeric user id of the operator, recorded in the audit event")
	flags.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "usage: elitea-main %s --user USER (--sub NEW_SUB | --clear) [--old-sub OLD_SUB] [--apply --operator OPERATOR_ID]\n", oidcRelinkCommand)
		_, _ = fmt.Fprintln(stderr, "reads DATABASE_URL")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return repos.OIDCRelinkRequest{}, false
	}
	req := repos.OIDCRelinkRequest{
		User: strings.TrimSpace(*user), NewSubject: *sub, Clear: *clearLink, OldSubject: *oldSub,
		Apply: *apply, Operator: strings.TrimSpace(*operator),
	}
	switch {
	case req.User == "":
		_, _ = fmt.Fprintln(stderr, "--user is required")
	case req.Clear == (req.NewSubject != ""):
		_, _ = fmt.Fprintln(stderr, "give exactly one of --sub or --clear")
	case !req.Apply && req.Operator != "":
		_, _ = fmt.Fprintln(stderr, "--operator only applies together with --apply")
	case req.Apply && req.Operator == "":
		_, _ = fmt.Fprintln(stderr, "--apply needs --operator: the numeric user id of the operator who makes the change")
	case !req.Apply:
		return req, true
	default:
		_, canonical, err := repos.CanonicalUserID(req.Operator)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "--operator must be a numeric user id")
			return repos.OIDCRelinkRequest{}, false
		}
		req.Operator = canonical
		return req, true
	}
	return repos.OIDCRelinkRequest{}, false
}

func runOIDCRelinkWith(ctx context.Context, args []string, lookup func(string) (string, bool), stdout, stderr io.Writer, connect oidcRelinkConnect) int {
	req, ok := parseOIDCRelinkArgs(args, stderr)
	if !ok {
		return 2
	}
	dsn, _ := lookup("DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		_, _ = fmt.Fprintln(stderr, "DATABASE_URL is required")
		return 2
	}
	ctx, cancel := context.WithTimeout(ctx, oidcRelinkTimeout)
	defer cancel()
	pool, err := connect(ctx, dsn)
	if err != nil {
		// pgx's error can quote the DSN; say what failed, not with what.
		_, _ = fmt.Fprintln(stderr, "connect to the platform database (DATABASE_URL) failed")
		return 1
	}
	defer pool.Close()
	if req.Apply {
		known, err := operatorExists(ctx, pool, req.Operator)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "oidc-relink stopped: read the operator:", err)
			return 1
		}
		if !known {
			_, _ = fmt.Fprintf(stderr, "--operator %s is not a user id\n", req.Operator)
			return 2
		}
	}

	var plan repos.OIDCRelinkPlan
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		plan, err = repos.RelinkOIDCIdentityTx(ctx, tx, req)
		return err
	})
	if plan.UserID != 0 {
		printOIDCRelinkPlan(stdout, plan)
	}
	switch {
	case err == nil:
	case errors.Is(err, repos.ErrRelinkUserNotFound), errors.Is(err, repos.ErrRelinkAmbiguousUser),
		errors.Is(err, repos.ErrRelinkAmbiguousLink), errors.Is(err, repos.ErrRelinkOldLinkNotFound),
		errors.Is(err, repos.ErrRelinkSubjectTaken), errors.Is(err, repos.ErrRelinkInvalidSubject),
		errors.Is(err, repos.ErrRelinkInvalidOperation):
		_, _ = fmt.Fprintln(stderr, "refused:", err)
		return 2
	default:
		_, _ = fmt.Fprintln(stderr, "oidc-relink failed; nothing was changed:", err)
		return 1
	}
	switch {
	case plan.NoChange:
		_, _ = fmt.Fprintln(stdout, "nothing to change")
	case plan.Applied:
		_, _ = fmt.Fprintf(stdout, "applied; security audit event %s\n", plan.AuditEventID)
	default:
		_, _ = fmt.Fprintln(stdout, "dry run: nothing was changed. To apply, re-run with --apply --operator OPERATOR_ID.")
	}
	return 0
}

func printOIDCRelinkPlan(w io.Writer, plan repos.OIDCRelinkPlan) {
	_, _ = fmt.Fprintf(w, "user %d <%s>", plan.UserID, plan.Email)
	if plan.Suspended {
		_, _ = fmt.Fprint(w, " (suspended: a login stays refused until the account is unsuspended)")
	}
	_, _ = fmt.Fprintln(w)
	if len(plan.Links) == 0 {
		_, _ = fmt.Fprintln(w, "  current OIDC links: none")
	} else {
		_, _ = fmt.Fprintf(w, "  current OIDC links: oidc:%s\n", strings.Join(plan.Links, ", oidc:"))
	}
	if plan.Remove != "" {
		_, _ = fmt.Fprintf(w, "  remove: oidc:%s\n", plan.Remove)
	}
	if plan.Add != "" {
		_, _ = fmt.Fprintf(w, "  add:    oidc:%s\n", plan.Add)
	}
}
