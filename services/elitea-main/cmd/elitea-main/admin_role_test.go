package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func mustNotConnect(connected *bool) oidcRelinkConnect {
	return func(context.Context, string) (*pgxpool.Pool, error) {
		*connected = true
		return nil, errors.New("must not connect")
	}
}

// The argument rules of `elitea-main admin-role` are decided before any
// database connection: a refused invocation never connects, and exits 2.
func TestAdminRoleRefusesBadInvocationsBeforeConnecting(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no sub-command":           {nil, "usage:"},
		"unknown sub-command":      {[]string{"promote", "7", "admin"}, `unknown sub-command "promote"`},
		"grant without a role":     {[]string{"grant", "7"}, "grant takes 2 argument(s), got 1"},
		"grant with too many":      {[]string{"grant", "7", "admin", "extra"}, "grant takes 2 argument(s), got 3"},
		"revoke without arguments": {[]string{"revoke"}, "revoke takes 2 argument(s), got 0"},
		"reset without a role":     {[]string{"reset"}, "reset takes 1 argument(s), got 0"},
		"list with an argument":    {[]string{"list", "admin"}, "list takes 0 argument(s), got 1"},
		"override on grant":        {[]string{"grant", "7", "admin", "--override-last-administrator"}, "only applies to revoke"},
		"override on list":         {[]string{"list", "--override-last-administrator"}, "only applies to revoke and reset"},
		"positional after --":      {[]string{"grant", "--", "a@example.com", "admin", "--reason", "x"}, "grant takes 2 argument(s), got 4"},
		"reason too long in runes": {[]string{"grant", "7", "admin", "--reason", strings.Repeat("é", 501)}, "at most 500"},
		"reason on reset":          {[]string{"reset", "admin", "--reason", "x"}, "--reason only applies"},
		"reason on two lines":      {[]string{"grant", "7", "admin", "--reason", "a\nb"}, "on one line"},
		"reason too long":          {[]string{"grant", "7", "admin", "--reason", strings.Repeat("x", 501)}, "at most 500"},
		"empty user":               {[]string{"grant", " ", "admin"}, "USER is required"},
		"empty role":               {[]string{"reset", " "}, "ROLE is required"},
		"unknown flag":             {[]string{"revoke", "7", "admin", "--force"}, "flag provided but not defined"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			connected := false
			code := runAdminRoleWith(context.Background(), tc.args,
				func(string) (string, bool) { return "postgres://unused", true }, &stdout, &stderr,
				mustNotConnect(&connected))
			if code != adminRoleExitUsage || connected {
				t.Fatalf("code = %d, connected = %v, stderr = %s", code, connected, stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr.String(), tc.want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("a refused invocation printed to stdout: %q", stdout.String())
			}
		})
	}
}

// Flags may come before, between or after the positional arguments.
func TestAdminRoleAcceptsFlagsAnywhere(t *testing.T) {
	for _, args := range [][]string{
		{"revoke", "--override-last-administrator", "--reason", "left the company", "a@example.com", "super_admin"},
		{"revoke", "a@example.com", "--reason", "left the company", "super_admin", "--override-last-administrator"},
		{"revoke", "a@example.com", "super_admin", "--override-last-administrator", "--reason=left the company"},
	} {
		var stderr bytes.Buffer
		req, ok := parseAdminRoleArgs(args, &stderr)
		if !ok {
			t.Fatalf("%v refused: %s", args, stderr.String())
		}
		want := adminRoleRequest{sub: adminRoleRevoke, user: "a@example.com", role: "super_admin",
			reason: "left the company", override: true}
		if req != want {
			t.Fatalf("%v parsed as %+v, want %+v", args, req, want)
		}
	}
	var stderr bytes.Buffer
	// A reason of 500 non-ASCII characters is within the limit (characters, not bytes).
	if _, ok := parseAdminRoleArgs([]string{"grant", "7", "admin", "--reason", strings.Repeat("é", 500)}, &stderr); !ok {
		t.Fatalf("a 500-character reason was refused: %s", stderr.String())
	}
	if req, ok := parseAdminRoleArgs([]string{"grant", "--", "a@example.com", "admin"}, &stderr); !ok ||
		req.user != "a@example.com" || req.role != "admin" {
		t.Fatalf("-- refused: %+v %s", req, stderr.String())
	}
	if req, ok := parseAdminRoleArgs([]string{"list"}, &stderr); !ok || req.sub != adminRoleList {
		t.Fatalf("list refused: %s", stderr.String())
	}
	if req, ok := parseAdminRoleArgs([]string{"reset", "admin"}, &stderr); !ok || req.role != "admin" {
		t.Fatalf("reset refused: %s", stderr.String())
	}
}

func TestAdminRoleNeedsTheDatabaseURL(t *testing.T) {
	var stdout, stderr bytes.Buffer
	connected := false
	code := runAdminRoleWith(context.Background(), []string{"list"},
		func(string) (string, bool) { return "", false }, &stdout, &stderr, mustNotConnect(&connected))
	if code != adminRoleExitUsage || connected || !strings.Contains(stderr.String(), "DATABASE_URL is required") {
		t.Fatalf("code = %d, connected = %v, stderr = %q", code, connected, stderr.String())
	}
}

// A failed connection exits 1 and does not print the DSN, which can hold the
// database password.
func TestAdminRoleConnectFailureExitsOneWithoutTheDSN(t *testing.T) {
	var stdout, stderr bytes.Buffer
	const dsn = "postgres://operator:s3cret@db.invalid/elitea"
	code := runAdminRoleWith(context.Background(), []string{"grant", "7", "admin"},
		func(string) (string, bool) { return dsn, true }, &stdout, &stderr,
		func(context.Context, string) (*pgxpool.Pool, error) { return nil, errors.New("dial " + dsn) })
	if code != adminRoleExitFailed {
		t.Fatalf("code = %d, want %d", code, adminRoleExitFailed)
	}
	if strings.Contains(stderr.String(), "s3cret") || !strings.Contains(stderr.String(), "connect to the platform database") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
