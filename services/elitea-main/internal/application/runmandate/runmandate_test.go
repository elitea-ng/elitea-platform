package runmandate_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/runmandate/runmandatetest"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
)

const tenant = int64(7)

func scheduleTarget(cron, input string) runmandate.Target {
	return runmandate.Target{
		Origin: runmandate.OriginPipelineSchedule, TenantProjectID: tenant,
		ResourceID: "3", ApplicationVersionID: 11, Cron: cron, Input: &input,
	}
}

func fixture() *runmandatetest.Store {
	store := runmandatetest.New()
	store.PutBinding("binding-1", tenant, 11)
	store.PutMandate("auto", tenant, mandate.KindAutonomous, "binding-1", 0)
	store.PutMandate("background", tenant, mandate.KindDelegatedBackground, "binding-1", 42)
	return store
}

func TestAdmitNamesTheMandateAndItsSubject(t *testing.T) {
	service := fixture().Service()
	authority, err := service.Admit(context.Background(), "background", scheduleTarget("0 * * * *", "x"))
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if authority.MandateID != "background" || authority.MandateEpoch != 2 || authority.SubjectUserID != 42 {
		t.Fatalf("authority = %+v", authority)
	}
	authority, err = service.Admit(context.Background(), "auto", scheduleTarget("0 * * * *", "x"))
	if err != nil || authority.SubjectUserID != 0 || authority.MandateID != "auto" {
		t.Fatalf("autonomous authority = %+v, %v", authority, err)
	}
}

func TestAdmitRefusals(t *testing.T) {
	target := scheduleTarget("0 * * * *", "x")
	cases := []struct {
		name   string
		id     string
		change func(*runmandatetest.Store)
		target runmandate.Target
		code   string
	}{
		{name: "no mandate", id: "", code: runmandate.CodeMandateRequired},
		{name: "unknown mandate", id: "missing", code: runmandate.CodeMandateNotFound},
		{name: "revoked", id: "auto", code: runmandate.CodeMandateNotUsable, change: func(s *runmandatetest.Store) {
			s.Update("auto", func(m *mandate.Mandate) { m.State = mandate.StateRevoked; m.RevocationEpoch = 3 })
		}},
		{name: "expired", id: "auto", code: runmandate.CodeMandateNotUsable, change: func(s *runmandatetest.Store) {
			s.Update("auto", func(m *mandate.Mandate) { m.ExpiresAt = time.Now().Add(-time.Minute) })
		}},
		{name: "suspended", id: "auto", code: runmandate.CodeMandateNotUsable, change: func(s *runmandatetest.Store) {
			s.Update("auto", func(m *mandate.Mandate) { m.State = mandate.StateSuspended })
		}},
		{name: "mandate epoch moved", id: "auto", code: runmandate.CodeMandateNotUsable, change: func(s *runmandatetest.Store) {
			s.Epochs = map[string]int64{"auto": 3}
		}},
		{name: "binding epoch revoked", id: "auto", code: runmandate.CodeMandateNotUsable, change: func(s *runmandatetest.Store) {
			s.Epochs = map[string]int64{"binding-1": -2}
		}},
		{name: "binding revoked", id: "auto", code: runmandate.CodeMandateNotUsable, change: func(s *runmandatetest.Store) {
			b := s.Bindings["binding-1"]
			b.State = agentidentity.BindingRevoked
			s.Bindings["binding-1"] = b
		}},
		{name: "another tenant", id: "auto", code: runmandate.CodeMandateTenantMismatch, change: func(s *runmandatetest.Store) {
			s.Update("auto", func(m *mandate.Mandate) { m.TenantProjectID = tenant + 1 })
		}},
		{name: "interactive delegation", id: "auto", code: runmandate.CodeMandateKindNotAllowed, change: func(s *runmandatetest.Store) {
			s.Update("auto", func(m *mandate.Mandate) { m.Kind = mandate.Kind("interactive_delegation") })
		}},
		{name: "kind not in the policy", id: "auto", code: runmandate.CodeMandateKindNotAllowed, change: func(s *runmandatetest.Store) {
			s.Kinds = []string{"delegated_background"}
		}},
		{name: "another version", id: "auto", code: runmandate.CodeMandateBindingMismatch,
			target: runmandate.Target{Origin: runmandate.OriginPipelineSchedule, TenantProjectID: tenant, ResourceID: "3", ApplicationVersionID: 12}},
		{name: "out of scope", id: "auto", code: runmandate.CodeMandateScopeMismatch, change: func(s *runmandatetest.Store) {
			s.Update("auto", func(m *mandate.Mandate) { m.ResourceScopes = []string{runmandate.CronScope("30 2 * * *")} })
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := fixture()
			if tc.change != nil {
				tc.change(store)
			}
			got := target
			if tc.target.Origin != "" {
				got = tc.target
			}
			service := store.Service()
			_, err := service.Admit(context.Background(), tc.id, got)
			if code := runmandate.Code(err); code != tc.code {
				t.Fatalf("code = %q (%v), want %q", code, err, tc.code)
			}
			if err := service.Refuse(context.Background(), got, tc.id, err); err != nil {
				t.Fatal(err)
			}
			refusals := store.Refusals()
			if len(refusals) != 1 || refusals[0].Code != tc.code || refusals[0].MandateID != tc.id {
				t.Fatalf("refusals = %+v", refusals)
			}
		})
	}
}

func TestCheckAttach(t *testing.T) {
	store := fixture()
	store.Update("background", func(m *mandate.Mandate) { m.State = mandate.StateProposed })
	service := store.Service()
	target := scheduleTarget("0 * * * *", "x")
	if _, err := service.CheckAttach(context.Background(), "background", target); err != nil {
		t.Fatalf("a proposed mandate may be attached: %v", err)
	}
	store.Update("auto", func(m *mandate.Mandate) { m.State = mandate.StateRevoked })
	if _, err := service.CheckAttach(context.Background(), "auto", target); !errors.Is(err, runmandate.ErrMandateNotUsable) {
		t.Fatalf("revoked attach = %v", err)
	}
	store.PutMandate("interactive", tenant, mandate.Kind("interactive_delegation"), "binding-1", 42)
	if _, err := service.CheckAttach(context.Background(), "interactive", target); !errors.Is(err, runmandate.ErrKindNotAllowed) {
		t.Fatalf("interactive attach = %v", err)
	}
	store.PutMandate("foreign", tenant+1, mandate.KindAutonomous, "binding-1", 0)
	if _, err := service.CheckAttach(context.Background(), "foreign", target); !errors.Is(err, runmandate.ErrTenantMismatch) {
		t.Fatalf("foreign attach = %v", err)
	}
}

func TestCoversCronAndInputScopes(t *testing.T) {
	store := fixture()
	store.Update("auto", func(m *mandate.Mandate) {
		m.ResourceScopes = []string{runmandate.CronScope("0  *  * * *"), runmandate.CronScope("15 * * * *"), runmandate.InputScope("x")}
	})
	service := store.Service()
	for _, tc := range []struct {
		cron, input string
		want        error
	}{
		{"0 * * * *", "x", nil},
		{"15 * * * *", "x", nil},
		{"30 * * * *", "x", runmandate.ErrScopeMismatch},
		{"0 * * * *", "y", runmandate.ErrScopeMismatch},
	} {
		if err := service.Covers(context.Background(), "auto", scheduleTarget(tc.cron, tc.input)); !errors.Is(err, tc.want) {
			t.Fatalf("Covers(%q, %q) = %v, want %v", tc.cron, tc.input, err, tc.want)
		}
	}
	if err := service.Covers(context.Background(), "background", scheduleTarget("1 2 3 4 5", "any")); err != nil {
		t.Fatalf("no scope prefix is unrestricted: %v", err)
	}
	if err := service.Covers(context.Background(), "missing", scheduleTarget("0 * * * *", "x")); !errors.Is(err, runmandate.ErrMandateNotFound) {
		t.Fatalf("missing = %v", err)
	}
}
