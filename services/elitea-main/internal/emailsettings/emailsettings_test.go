package emailsettings

// The LAYERING rule and the completeness rule, tested without a database.
//
// Both are the behaviour gap G7 is about, and both are invisible to a status
// code:
//
//  1. **Database over environment, FIELD BY FIELD.** A whole-document override
//     would let a half-filled form discard the chart's sender address, and the
//     operator would find out from a message that never arrived.
//  2. **A merged document decides whether anything can be sent.** A layer is
//     allowed to be incomplete; the merge is not. This is what stops the
//     invite handlers from reporting a delivery that never happened.
//  3. **The refusal NAMES the field.** "Not configured" is true of every
//     incomplete document and actionable for none of them.

import (
	"context"
	"errors"
	"testing"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/mailer"
)

// fakeSource stands in for the database layer.
type fakeSource struct {
	ready    bool
	settings Settings
	password string
	loadErr  error
}

func (f fakeSource) Ready() bool { return f.ready }
func (f fakeSource) Load(context.Context) (Settings, bool, error) {
	if f.loadErr != nil {
		return Settings{}, false, f.loadErr
	}
	return f.settings, f.password != "", nil
}
func (f fakeSource) Password(context.Context) (string, error) {
	if f.loadErr != nil {
		return "", f.loadErr
	}
	return f.password, nil
}

func envLayer() Settings {
	return Settings{
		Host:          "smtp.chart.example",
		Port:          25,
		TLS:           TLSNone,
		From:          "chart@acme.example",
		ReplyTo:       "chart-help@acme.example",
		PublicBaseURL: "https://chart.acme.example",
	}
}

func TestResolveDatabaseOverEnvironmentPerField(t *testing.T) {
	// The stored layer states the HOST and the sender only. Everything it does
	// not state must still come from the environment: this is the shape of an
	// operator who corrected one relay host on the admin page and left the
	// chart alone.
	source := fakeSource{ready: true, settings: Settings{
		Host: "smtp.acme.example",
		From: "noreply@acme.example",
	}}
	got := NewResolver(source, envLayer(), "").Resolve(context.Background())

	if !got.Configured {
		t.Fatalf("not configured: %s", got.Reason)
	}
	if got.Effective.Host != "smtp.acme.example" || got.Effective.From != "noreply@acme.example" {
		t.Errorf("the database did not win: %+v", got.Effective)
	}
	if got.Effective.Port != 25 || got.Effective.TLS != TLSNone ||
		got.Effective.ReplyTo != "chart-help@acme.example" ||
		got.Effective.PublicBaseURL != "https://chart.acme.example" {
		t.Errorf("a field the database did not state lost its environment value: %+v", got.Effective)
	}
	// The tags the admin page renders beside each control.
	sources := Sources(got.Stored, got.Effective)
	for field, want := range map[string]string{
		"host":            SourceDatabase,
		"from":            SourceDatabase,
		"port":            SourceEnvironment,
		"tls":             SourceEnvironment,
		"reply_to":        SourceEnvironment,
		"public_base_url": SourceEnvironment,
		"username":        SourceUnset,
	} {
		if sources[field] != want {
			t.Errorf("source[%s] = %q, want %q", field, sources[field], want)
		}
	}
	// The transport carries the merged values, not one layer's.
	if got.Config.Host != "smtp.acme.example" || got.Config.Port != 25 || got.Config.TLS != mailer.TLSNone {
		t.Errorf("transport config = %+v", got.Config)
	}
}

func TestResolvePasswordPrefersTheVault(t *testing.T) {
	env := envLayer()
	env.Username = "chart-user"
	source := fakeSource{ready: true, settings: Settings{Username: "vault-user"}, password: "sealed"}

	got := NewResolver(source, env, "from-the-chart").Resolve(context.Background())
	if !got.Configured {
		t.Fatalf("not configured: %s", got.Reason)
	}
	if got.Config.Password != "sealed" || got.Config.Username != "vault-user" {
		t.Errorf("the vault did not win: %+v", got.Config)
	}
	if !got.PasswordSet || got.PasswordSource != SourceDatabase {
		t.Errorf("password reported as %v/%s", got.PasswordSet, got.PasswordSource)
	}
}

func TestResolveFallsBackToTheEnvironmentWhenTheStoreFails(t *testing.T) {
	// A database hiccup must not turn a working relay into a deployment that
	// reports every invitation undelivered. The environment layer is what the
	// process booted with, so this is a fall back to the previous behaviour.
	source := fakeSource{ready: true, loadErr: errors.New("connection refused")}
	got := NewResolver(source, envLayer(), "").Resolve(context.Background())
	if !got.Configured {
		t.Fatalf("a store failure took a working environment relay down: %s", got.Reason)
	}
	if got.Config.Host != "smtp.chart.example" {
		t.Errorf("config = %+v", got.Config)
	}
}

func TestResolveIncompleteNamesTheField(t *testing.T) {
	for name, testCase := range map[string]struct {
		env       Settings
		stored    Settings
		wantField string
	}{
		"nothing anywhere": {
			wantField: "",
		},
		"a host with no sender": {
			stored:    Settings{Host: "smtp.acme.example", PublicBaseURL: "https://acme.example"},
			wantField: "from",
		},
		"a host with no public origin": {
			stored:    Settings{Host: "smtp.acme.example", From: "noreply@acme.example"},
			wantField: "public_base_url",
		},
		"a user name with no password": {
			stored: Settings{
				Host: "smtp.acme.example", From: "noreply@acme.example",
				PublicBaseURL: "https://acme.example", Username: "u",
			},
			wantField: "password",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := NewResolver(fakeSource{ready: true, settings: testCase.stored}, testCase.env, "").
				Resolve(context.Background())
			if got.Configured {
				t.Fatal("an incomplete document reported itself sendable")
			}
			if got.Reason == "" {
				t.Fatal("no reason: the operator is told nothing about what to set")
			}
			if testCase.wantField == "" {
				if got.Reason != NotConfiguredReason {
					t.Errorf("reason = %q, want the generic one", got.Reason)
				}
				return
			}
			// The generic sentence is not good enough once a specific field is
			// known: it is true of every one of these cases.
			if got.Reason == NotConfiguredReason {
				t.Errorf("reason is the generic one for a specific failure (%s)", testCase.wantField)
			}
		})
	}
}

func TestTransportIsNullWhenNothingIsConfigured(t *testing.T) {
	transport, base, ok := NewResolver(fakeSource{}, Settings{}, "").Transport(context.Background())
	if ok {
		t.Fatal("an unconfigured deployment reported a usable transport")
	}
	if base != "" {
		t.Errorf("base URL = %q", base)
	}
	if _, isNull := transport.(mailer.NullTransport); !isNull {
		t.Errorf("transport = %T, want the null one", transport)
	}
}

func TestParseAndFormatTLSRoundTrip(t *testing.T) {
	// `implicit` is the transport's word for port 465 and `tls` is this
	// surface's. Both must parse, and the surface must publish exactly one of
	// them, or the chart and the admin page would disagree about the same
	// relay.
	for _, spelling := range []string{"tls", "implicit", "TLS", " Implicit "} {
		mode, ok := ParseTLS(spelling)
		if !ok || mode != mailer.TLSImplicit {
			t.Fatalf("ParseTLS(%q) = %v, %v", spelling, mode, ok)
		}
		if FormatTLS(mode) != TLSImplicit {
			t.Fatalf("FormatTLS did not normalise %q", spelling)
		}
	}
	if _, ok := ParseTLS("ssl3"); ok {
		t.Error("an unknown TLS mode was accepted")
	}
	if mode, ok := ParseTLS(""); !ok || mode != "" {
		t.Errorf("the empty string must stay unstated, got %q, %v", mode, ok)
	}
}

func TestValidateRefusesAValueAndNamesIt(t *testing.T) {
	for name, testCase := range map[string]struct {
		settings  Settings
		wantField string
	}{
		"port out of range":  {Settings{Port: 70000}, "port"},
		"unknown tls":        {Settings{TLS: "ssl3"}, "tls"},
		"sender not address": {Settings{From: "not an address"}, "from"},
		"reply not address":  {Settings{ReplyTo: "not an address"}, "reply_to"},
		"relative origin":    {Settings{PublicBaseURL: "/elitea"}, "public_base_url"},
	} {
		t.Run(name, func(t *testing.T) {
			err := testCase.settings.Validate()
			var field FieldError
			if !errors.As(err, &field) {
				t.Fatalf("err = %v, want a FieldError", err)
			}
			if field.Field != testCase.wantField {
				t.Errorf("field = %q, want %q", field.Field, testCase.wantField)
			}
		})
	}
	// A layer is allowed to be incomplete: every field is optional here,
	// because the layer under it may supply what this one omits.
	if err := (Settings{Host: "smtp.acme.example"}).Validate(); err != nil {
		t.Errorf("a partial layer was refused: %v", err)
	}
}

func TestTrimNormalisesWhatAFormSubmits(t *testing.T) {
	got := Settings{
		Host:          "  smtp.acme.example ",
		TLS:           " StartTLS ",
		PublicBaseURL: " https://acme.example/ ",
	}.Trim()
	if got.Host != "smtp.acme.example" || got.TLS != "starttls" || got.PublicBaseURL != "https://acme.example" {
		t.Fatalf("got %+v", got)
	}
}
