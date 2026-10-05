package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/authcomposition"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/identityrepo"
)

func TestLogFormUserConfigurationNamesTheRefusedLogins(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buffer, nil))

	logFormUserConfiguration(logger, authcomposition.FormUserReport{Configured: 3})
	if buffer.Len() != 0 {
		t.Fatalf("a clean configuration logged %q", buffer.String())
	}

	logFormUserConfiguration(logger, authcomposition.FormUserReport{
		Configured: 3, MisconfiguredLogins: []string{"alice", "bob"},
	})
	logged := buffer.String()
	for _, want := range []string{"level=WARN", "will be refused at sign-in", "logins=\"[alice bob]\"", "misconfigured=2"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log %q lacks %q", logged, want)
		}
	}
}

func TestLogReservedDomainSignInAccountsWarnsOnlyWhenFound(t *testing.T) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buffer, nil))

	logReservedDomainSignInAccounts(logger, identityrepo.ReservedDomainAccounts{})
	if buffer.Len() != 0 {
		t.Fatalf("nothing found, yet logged %q", buffer.String())
	}
	logReservedDomainSignInAccounts(logger, identityrepo.ReservedDomainAccounts{
		Count: 2, ProviderReferences: []string{"alice", "carol"},
	})
	logged := buffer.String()
	for _, want := range []string{"level=WARN", "reserved @centry.user domain", "accounts=2", "UPGRADING.md"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log %q lacks %q", logged, want)
		}
	}
}
