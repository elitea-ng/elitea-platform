package emailsettings

// The RESOLUTION half of the package: database over environment, evaluated
// once per send.
//
// Nothing is cached. That is the same decision `internal/platformconfig` makes
// and for the same reason: the alternative reads the value into process state
// at start-up and then needs a restart signal to pick up a change, and this
// platform has no restart signal to offer. Resolution is one indexed query on
// a seven-row section plus one vault read, and a deployment sends far fewer
// messages per second than it serves requests.
//
// A resolution that cannot read the database FALLS BACK to the environment
// rather than failing. A database hiccup must not turn a working relay into a
// deployment that reports every invitation undelivered — the environment layer
// is exactly the value the process booted with, so falling back to it is
// falling back to the previous behaviour.

import (
	"context"
	"log/slog"
	"net/mail"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/mailer"
)

// Source is the READ seam over the stored layer.
//
// It is an interface rather than `*Store` so that the layering rule — database
// over environment, field by field — can be tested without a database. That
// rule is the whole behaviour of this file, and a test that needs PostgreSQL
// to exercise it is a test that gets skipped and then believed.
type Source interface {
	// Ready reports whether the stored layer can be read at all.
	Ready() bool
	// Load reads the stored layer and whether a password is sealed.
	Load(ctx context.Context) (Settings, bool, error)
	// Password reads the sealed plaintext, or the empty string.
	Password(ctx context.Context) (string, error)
}

// Resolver merges the environment defaults with the stored layer and builds a
// transport for one send.
type Resolver struct {
	source Source
	env    Settings
	// envPassword is SMTP_PASSWORD. It is the fallback for the vault entry,
	// never the other way round.
	envPassword string
}

// NewResolver wires the resolver. A nil source makes every resolution the
// environment's, which is the behaviour of the deployment before this package
// existed.
func NewResolver(source Source, env Settings, envPassword string) *Resolver {
	return &Resolver{source: source, env: env, envPassword: envPassword}
}

// Store returns the concrete store this resolver reads, so a composition root
// can give the WRITE surface and the READ path the same one. Two stores on two
// pools is how a write path and a read path come to disagree about which
// database holds the value — the rule `internal/api/router.go` states beside
// the shared vault handler.
//
// It answers nil for a resolver built over a test double, which has no writes
// to share.
func (r *Resolver) Store() *Store {
	if r == nil {
		return nil
	}
	store, _ := r.source.(*Store)
	return store
}

// Resolution is one send's effective configuration.
type Resolution struct {
	// Config is the merged transport configuration. It is meaningful only when
	// Configured is true.
	Config mailer.Config
	// PublicBaseURL is the origin every link and the logo are absolutised
	// against.
	PublicBaseURL string
	// Configured reports whether a message can actually be submitted.
	Configured bool
	// Reason says why not, when Configured is false. It is shown to the
	// operator, so it names what to set rather than what failed internally.
	Reason string
	// Stored is the database layer as read, for the admin surface. It never
	// carries the password.
	Stored Settings
	// Effective is the merged document, for the admin surface's source tags.
	Effective Settings
	// PasswordSet reports whether a password is in force, from either layer.
	PasswordSet bool
	// PasswordSource is which layer supplied it.
	PasswordSource string
}

// NotConfiguredReason is what a deployment with no relay anywhere answers. It
// is a statement about the DEPLOYMENT, not about the request, and it names the
// two places an operator can fix it.
const NotConfiguredReason = "outbound e-mail is not configured on this deployment: set an SMTP host on " +
	"Admin › E-mail, or set SMTP_HOST in the environment"

// Resolve merges the layers and reports whether the result can send.
func (r *Resolver) Resolve(ctx context.Context) Resolution {
	stored := Settings{}
	password := r.envPassword
	passwordSource := SourceUnset
	if password != "" {
		passwordSource = SourceEnvironment
	}

	if r.source != nil && r.source.Ready() {
		loaded, _, err := r.source.Load(ctx)
		if err != nil {
			// The environment layer is what the process booted with, so this
			// is a fall back to the previous behaviour rather than a failure.
			slog.Warn("outbound e-mail settings could not be read; using the environment defaults",
				"error", err)
		} else {
			stored = loaded
			sealed, err := r.source.Password(ctx)
			switch {
			case err != nil:
				slog.Warn("the stored SMTP password could not be read; using the environment default",
					"error", err)
			case sealed != "":
				password = sealed
				passwordSource = SourceDatabase
			}
		}
	}

	effective := Layered(r.env, stored)
	resolution := Resolution{
		PublicBaseURL:  effective.PublicBaseURL,
		Stored:         stored,
		Effective:      effective,
		PasswordSet:    password != "",
		PasswordSource: passwordSource,
	}

	if effective.Host == "" {
		resolution.Reason = NotConfiguredReason
		return resolution
	}
	config, err := transportConfig(effective, password)
	if err != nil {
		resolution.Reason = err.Error()
		return resolution
	}
	resolution.Config = config
	resolution.Configured = true
	return resolution
}

// Transport builds the transport for one send, or reports why it cannot.
func (r *Resolver) Transport(ctx context.Context) (mailer.Transport, string, bool) {
	resolution := r.Resolve(ctx)
	if !resolution.Configured {
		return mailer.NullTransport{}, resolution.PublicBaseURL, false
	}
	transport, err := mailer.New(resolution.Config)
	if err != nil {
		slog.Warn("the resolved outbound e-mail configuration was refused by the transport", "error", err)
		return mailer.NullTransport{}, resolution.PublicBaseURL, false
	}
	return transport, resolution.PublicBaseURL, true
}

// transportConfig turns a merged document into a transport configuration, or
// names the field that stops it.
//
// The COMPLETENESS rules live here rather than on Settings.Validate because a
// layer is allowed to be incomplete and a merged document is not. The messages
// are the operator's, so each one names a control on the page.
func transportConfig(settings Settings, password string) (mailer.Config, error) {
	mode, ok := ParseTLS(settings.TLS)
	if !ok {
		return mailer.Config{}, FieldError{Field: "tls", Reason: "the TLS mode must be none, starttls or tls"}
	}
	if mode == "" {
		mode = mailer.TLSStartTLS
	}
	port := settings.Port
	if port == 0 {
		// The transport's own default, restated here because the merged
		// document must be complete before it reaches New.
		port = 587
		if mode == mailer.TLSImplicit {
			port = 465
		}
	}
	if settings.From == "" {
		return mailer.Config{}, FieldError{
			Field:  "from",
			Reason: "a sender address is required before e-mail can be sent: set From on Admin › E-mail, or EMAIL_FROM",
		}
	}
	from, err := mail.ParseAddress(settings.From)
	if err != nil {
		return mailer.Config{}, FieldError{Field: "from", Reason: "the sender must be an address such as noreply@example.com"}
	}
	config := mailer.Config{
		Host:     settings.Host,
		Port:     port,
		Username: settings.Username,
		Password: password,
		TLS:      mode,
		From:     *from,
	}
	if settings.ReplyTo != "" {
		replyTo, err := mail.ParseAddress(settings.ReplyTo)
		if err != nil {
			return mailer.Config{}, FieldError{Field: "reply_to", Reason: "the reply-to must be an address such as help@example.com"}
		}
		config.ReplyTo = replyTo
	}
	if (config.Username == "") != (config.Password == "") {
		return mailer.Config{}, FieldError{
			Field: "password",
			Reason: "a user name and a password must be set together: the relay would refuse the session at AUTH, " +
				"and the message would be reported as sent",
		}
	}
	if settings.PublicBaseURL == "" {
		return mailer.Config{}, FieldError{
			Field: "public_base_url",
			Reason: "a public base URL is required before e-mail can be sent: the invitation link and the logo " +
				"need an absolute origin. Set it on Admin › E-mail, or PUBLIC_BASE_URL",
		}
	}
	return config, nil
}
