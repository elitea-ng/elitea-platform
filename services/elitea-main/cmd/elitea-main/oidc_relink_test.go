package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The flag rules of `elitea-main oidc-relink` are decided before any database
// connection: a refused invocation never connects.
func TestOIDCRelinkRefusesBadInvocationsBeforeConnecting(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no user":                 {[]string{"--clear"}, "--user is required"},
		"no operation":            {[]string{"--user", "7"}, "exactly one of --sub or --clear"},
		"both operations":         {[]string{"--user", "7", "--sub", "s", "--clear"}, "exactly one of --sub or --clear"},
		"operator without apply":  {[]string{"--user", "7", "--clear", "--operator", "1"}, "--operator only applies"},
		"apply without operator":  {[]string{"--user", "7", "--clear", "--apply"}, "--apply needs --operator"},
		"non-numeric operator":    {[]string{"--user", "7", "--clear", "--apply", "--operator", "admin"}, "numeric user id"},
		"positional argument":     {[]string{"--user", "7", "--clear", "extra"}, ""},
		"unknown flag is refused": {[]string{"--user", "7", "--clear", "--force"}, "flag provided but not defined"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			connected := false
			code := runOIDCRelinkWith(context.Background(), tc.args,
				func(string) (string, bool) { return "postgres://unused", true }, &stdout, &stderr,
				func(context.Context, string) (*pgxpool.Pool, error) {
					connected = true
					return nil, errors.New("must not connect")
				})
			if code != 2 || connected {
				t.Fatalf("code = %d, connected = %v, stderr = %s", code, connected, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestOIDCRelinkNeedsTheDatabaseURL(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runOIDCRelinkWith(context.Background(), []string{"--user", "7", "--clear"},
		func(string) (string, bool) { return "", false }, &stdout, &stderr,
		func(context.Context, string) (*pgxpool.Pool, error) { return nil, errors.New("must not connect") })
	if code != 2 || !strings.Contains(stderr.String(), "DATABASE_URL is required") {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}

func TestOIDCRelinkAcceptsAPlanAndAnApply(t *testing.T) {
	var stderr bytes.Buffer
	req, ok := parseOIDCRelinkArgs([]string{"--user", "a@example.com", "--sub", "new", "--old-sub", "old"}, &stderr)
	if !ok || req.Apply || req.NewSubject != "new" || req.OldSubject != "old" || req.User != "a@example.com" {
		t.Fatalf("plan request = %+v, ok = %v, stderr = %s", req, ok, stderr.String())
	}
	req, ok = parseOIDCRelinkArgs([]string{"--user", "7", "--clear", "--apply", "--operator", "007"}, &stderr)
	if !ok || !req.Apply || !req.Clear || req.Operator != "7" {
		t.Fatalf("apply request = %+v, ok = %v, stderr = %s", req, ok, stderr.String())
	}
}
