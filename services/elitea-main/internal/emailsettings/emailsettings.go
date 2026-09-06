// Package emailsettings is the DATABASE layer of outbound e-mail (gap G7).
//
// # Why this package exists
//
// Before it, mail transport was environment-only: SMTP_HOST, SMTP_PORT,
// SMTP_USERNAME, SMTP_PASSWORD, SMTP_TLS, EMAIL_FROM, EMAIL_REPLY_TO and
// PUBLIC_BASE_URL, read once at boot (cmd/elitea-main/mailer_config.go). Two
// consequences followed, and the second is the one that made invitations a
// feature nobody could turn on:
//
//   - an operator with no access to the deployment's environment could not
//     configure e-mail at all, so `POST /admin/user_invite/administration`
//     answered `invitation_delivered: false` on every install that did not
//     set the variables before the pods started; and
//   - a correction to a relay host needed a restart, because a value read at
//     boot cannot change without one.
//
// The admin Authentication surface solved the same shape of problem for
// identity providers, and this package copies its answer: the settings live in
// the database, the credential lives in the GLOBAL vault's hidden bucket, and
// resolution happens PER SEND, so a save takes effect on the next message
// rather than on the next deployment.
//
// # Resolution is per FIELD, and the database wins
//
// The environment stays the bootstrap default. A deployment that ships
// SMTP_HOST in its chart keeps working with no database row at all, and an
// operator who then sets a host in the admin page overrides that one field
// while EMAIL_FROM still comes from the chart. Per-field layering rather than
// whole-document layering is deliberate: a document-level override would make
// a half-filled form discard the environment's sender address, and the
// operator would find out from a refused message.
//
// Nothing here decides whether the merged result is USABLE. That question is
// answered by `internal/infra/mailer`.New on the merged config, so "configured"
// means the same thing on this path as it does on the boot path. A
// half-configured merge is not configured, and the invite handlers report that
// instead of claiming a delivery.
//
// # The password never comes back out
//
// Load reports whether a password is SEALED, never its value. The plaintext is
// read only by Resolve, on its way into one SMTP session. The hidden bucket
// keeps it out of the `{{secret.<name>}}` set every project workload resolves
// against — see `internal/api/v2/secrets/admin_hidden.go`.
package emailsettings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/secrets"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/mailer"
)

// Section is the `centry.platform_config` section these rows live in. It is
// named in `internal/platformconfig` beside the other sections for the reason
// they are named there: a rename must break the build rather than quietly
// return an empty map.
const Section = "email"

// The stored keys. They are the ENVIRONMENT variable names lowercased, so an
// operator reading a row and an operator reading the chart see the same word
// for the same thing.
const (
	KeyHost          = "smtp_host"
	KeyPort          = "smtp_port"
	KeyUsername      = "smtp_username"
	KeyTLS           = "smtp_tls"
	KeyFrom          = "email_from"
	KeyReplyTo       = "email_reply_to"
	KeyPublicBaseURL = "public_base_url"
)

// storedKeys is the full set. A write touches every one of them, so clearing a
// field in the form clears the row rather than leaving the previous value
// behind under a key the request did not mention.
var storedKeys = []string{KeyHost, KeyPort, KeyUsername, KeyTLS, KeyFrom, KeyReplyTo, KeyPublicBaseURL}

// The TLS vocabulary this surface speaks.
//
// It is not quite the transport's. `internal/infra/mailer` calls port 465
// `implicit`; every SMTP client an operator has configured calls it `tls`, and
// SMTP_TLS has accepted `implicit` since WP7. So the wire vocabulary is
// none/starttls/tls, `implicit` stays accepted as the synonym it is, and
// ParseTLS is the ONE place the two vocabularies meet.
const (
	TLSNone     = "none"
	TLSStartTLS = "starttls"
	TLSImplicit = "tls"
)

// ParseTLS maps a wire or environment spelling onto the transport's mode. The
// empty string means "not stated", which layering resolves.
func ParseTLS(value string) (mailer.TLSMode, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return "", true
	case TLSNone:
		return mailer.TLSNone, true
	case TLSStartTLS:
		return mailer.TLSStartTLS, true
	case TLSImplicit, string(mailer.TLSImplicit):
		return mailer.TLSImplicit, true
	default:
		return "", false
	}
}

// FormatTLS is ParseTLS's inverse: the word this surface publishes for one
// transport mode.
func FormatTLS(mode mailer.TLSMode) string {
	switch mode {
	case mailer.TLSNone:
		return TLSNone
	case mailer.TLSImplicit:
		return TLSImplicit
	case mailer.TLSStartTLS:
		return TLSStartTLS
	default:
		return ""
	}
}

// Settings is one LAYER of the configuration, not the resolved result.
//
// Every field is optional, and the zero value means "this layer does not state
// it". Port is an int with zero meaning unstated rather than a value with a
// default baked in: a stored zero and an absent row must layer identically, or
// the database would override the environment's port with nothing.
type Settings struct {
	Host          string `json:"host"`
	Port          int    `json:"port"`
	Username      string `json:"username"`
	TLS           string `json:"tls"`
	From          string `json:"from"`
	ReplyTo       string `json:"reply_to"`
	PublicBaseURL string `json:"public_base_url"`
}

// Empty reports whether this layer states nothing at all.
func (s Settings) Empty() bool {
	return s.Host == "" && s.Port == 0 && s.Username == "" && s.TLS == "" &&
		s.From == "" && s.ReplyTo == "" && s.PublicBaseURL == ""
}

// Trim normalises the values a form submits. Leading and trailing space in a
// host name is invisible in a text field and fatal at connect time.
func (s Settings) Trim() Settings {
	s.Host = strings.TrimSpace(s.Host)
	s.Username = strings.TrimSpace(s.Username)
	s.TLS = strings.ToLower(strings.TrimSpace(s.TLS))
	s.From = strings.TrimSpace(s.From)
	s.ReplyTo = strings.TrimSpace(s.ReplyTo)
	s.PublicBaseURL = strings.TrimRight(strings.TrimSpace(s.PublicBaseURL), "/")
	return s
}

// Validate refuses a layer that cannot be stored. It validates the values
// PRESENT; completeness is Resolve's question, because a field this layer
// omits may be supplied by the layer under it.
func (s Settings) Validate() error {
	if s.Port < 0 || s.Port > 65535 {
		return FieldError{Field: "port", Reason: "the port must be between 1 and 65535"}
	}
	if _, ok := ParseTLS(s.TLS); !ok {
		return FieldError{Field: "tls", Reason: "the TLS mode must be none, starttls or tls"}
	}
	if s.From != "" {
		if _, err := mail.ParseAddress(s.From); err != nil {
			return FieldError{Field: "from", Reason: "the sender must be an address such as noreply@example.com"}
		}
	}
	if s.ReplyTo != "" {
		if _, err := mail.ParseAddress(s.ReplyTo); err != nil {
			return FieldError{Field: "reply_to", Reason: "the reply-to must be an address such as help@example.com"}
		}
	}
	if s.PublicBaseURL != "" {
		parsed, err := url.Parse(s.PublicBaseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return FieldError{
				Field:  "public_base_url",
				Reason: "the public base URL must be an absolute http(s) origin, such as https://elitea.example.com",
			}
		}
	}
	// The username/password pair is NOT checked here: the password half is not
	// in this struct. Store.Save enforces the pair, where both halves are in
	// reach and where "what this write would leave in place" is knowable.
	return nil
}

// FieldError names the value that was refused. A refusal that does not say
// which field was wrong sends the operator back to a form of eight controls
// with nothing to go on.
type FieldError struct {
	Field  string
	Reason string
}

func (e FieldError) Error() string { return e.Reason }

// Layered merges two layers, with `over` winning FIELD BY FIELD.
func Layered(under, over Settings) Settings {
	merged := under
	if over.Host != "" {
		merged.Host = over.Host
	}
	if over.Port != 0 {
		merged.Port = over.Port
	}
	if over.Username != "" {
		merged.Username = over.Username
	}
	if over.TLS != "" {
		merged.TLS = over.TLS
	}
	if over.From != "" {
		merged.From = over.From
	}
	if over.ReplyTo != "" {
		merged.ReplyTo = over.ReplyTo
	}
	if over.PublicBaseURL != "" {
		merged.PublicBaseURL = over.PublicBaseURL
	}
	return merged
}

// Which layer decided one field.
const (
	SourceDatabase    = "database"
	SourceEnvironment = "environment"
	SourceUnset       = "unset"
)

// Sources reports, per field, which layer the effective value came from. The
// admin page renders it as the tag beside each control, so an operator can see
// that a blank field is not necessarily an unconfigured one.
//
// It is derived from the STORED and EFFECTIVE layers rather than from the
// environment layer directly. The two are equivalent — a field the database
// does not state takes the environment's value, so an effective value with no
// stored value came from the environment — and this way the page and the
// resolver cannot disagree about a field: they are reading the same two
// documents.
func Sources(stored, effective Settings) map[string]string {
	env := effective
	pick := func(fromDatabase, fromEnvironment bool) string {
		switch {
		case fromDatabase:
			return SourceDatabase
		case fromEnvironment:
			return SourceEnvironment
		default:
			return SourceUnset
		}
	}
	return map[string]string{
		"host":            pick(stored.Host != "", env.Host != ""),
		"port":            pick(stored.Port != 0, env.Port != 0),
		"username":        pick(stored.Username != "", env.Username != ""),
		"tls":             pick(stored.TLS != "", env.TLS != ""),
		"from":            pick(stored.From != "", env.From != ""),
		"reply_to":        pick(stored.ReplyTo != "", env.ReplyTo != ""),
		"public_base_url": pick(stored.PublicBaseURL != "", env.PublicBaseURL != ""),
	}
}

// SecretName is the vault entry the SMTP password is sealed under.
//
// It is DERIVED rather than stored, for the reason the identity provider
// surface derives its own: a stored reference and the row that names it can
// drift apart, and a derivation cannot.
func SecretName() string {
	return secrets.AdminHiddenSecretName("email", "smtp", "password")
}

// SecretStore is the vault seam. It is an interface so a test can prove that a
// refused save seals nothing, and so this package does not depend on how the
// secrets handler is constructed.
type SecretStore interface {
	StoreAdminHiddenSecret(ctx context.Context, name, value string) error
	LookupAdminHiddenSecret(ctx context.Context, name string) (string, error)
	DeleteAdminHiddenSecret(ctx context.Context, name string) error
}

// Store reads and writes the database layer.
type Store struct {
	pool  *pgxpool.Pool
	vault SecretStore
}

// NewStore wires the store. Either dependency may be absent; the surface that
// owns the routes answers 503 rather than pretending a read succeeded.
func NewStore(pool *pgxpool.Pool, vault SecretStore) *Store {
	return &Store{pool: pool, vault: vault}
}

// Ready reports whether both halves are wired.
func (s *Store) Ready() bool { return s != nil && s.pool != nil && s.vault != nil }

// Load reads the stored layer and whether a password is sealed.
func (s *Store) Load(ctx context.Context) (Settings, bool, error) {
	if s == nil || s.pool == nil {
		return Settings{}, false, errors.New("emailsettings: no database pool")
	}
	rows, err := s.pool.Query(ctx,
		`SELECT key, value FROM centry.platform_config WHERE section = $1`, Section)
	if err != nil {
		return Settings{}, false, err
	}
	defer rows.Close()

	stored := map[string]any{}
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return Settings{}, false, err
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return Settings{}, false, err
		}
		stored[key] = decoded
	}
	if err := rows.Err(); err != nil {
		return Settings{}, false, err
	}

	settings := Settings{
		Host:          stringOf(stored[KeyHost]),
		Port:          intOf(stored[KeyPort]),
		Username:      stringOf(stored[KeyUsername]),
		TLS:           stringOf(stored[KeyTLS]),
		From:          stringOf(stored[KeyFrom]),
		ReplyTo:       stringOf(stored[KeyReplyTo]),
		PublicBaseURL: stringOf(stored[KeyPublicBaseURL]),
	}
	sealed, err := s.PasswordSealed(ctx)
	if err != nil {
		return settings, false, err
	}
	return settings, sealed, nil
}

// noPasswordStored reports whether a vault lookup failed because NOTHING was
// ever stored, rather than because the vault would not open.
//
// Two absences answer that description, and the global vault produces the
// second one on every FRESH install:
//
//   - ErrSecretNotFound — the vault exists and holds no entry of this name; and
//   - ErrVaultAbsent — neither `centry.secrets_key` row nor `centry.secrets_data`
//     row exists yet, because no feature has sealed anything on this deployment.
//
// The second was read as a FAILURE. A fresh install with an SMTP host typed on
// Admin › E-mail and no password therefore logged "outbound e-mail settings
// could not be read" on every send and every page read, and the resolver
// discarded the stored rows for the environment defaults — a working relay
// reported as unreadable because a credential nobody set was missing.
//
// A vault that will not OPEN stays an error. "The operator has not set a
// password" and "this service cannot read its own vault" call for different
// messages, and collapsing THOSE is how a broken vault gets reported as a
// missing configuration.
func noPasswordStored(err error) bool {
	return errors.Is(err, secrets.ErrSecretNotFound) || errors.Is(err, secrets.ErrVaultAbsent)
}

// PasswordSealed reports whether the vault holds a password for this
// deployment.
func (s *Store) PasswordSealed(ctx context.Context) (bool, error) {
	if s == nil || s.vault == nil {
		return false, errors.New("emailsettings: no vault")
	}
	_, err := s.vault.LookupAdminHiddenSecret(ctx, SecretName())
	switch {
	case err == nil:
		return true, nil
	case noPasswordStored(err):
		return false, nil
	default:
		return false, err
	}
}

// Password reads the sealed plaintext, or the empty string when none is set.
// Only Resolve calls it, on its way into one SMTP session.
func (s *Store) Password(ctx context.Context) (string, error) {
	if s == nil || s.vault == nil {
		return "", errors.New("emailsettings: no vault")
	}
	value, err := s.vault.LookupAdminHiddenSecret(ctx, SecretName())
	switch {
	case err == nil:
		return value, nil
	case noPasswordStored(err):
		return "", nil
	default:
		return "", err
	}
}

// Save writes the stored layer and applies the password's THREE states.
//
// `password` is a pointer because absent, empty and a value mean three
// different things — leave the sealed value alone, clear it, and re-seal it.
// Collapsing absent and empty is how a save from a form that cannot echo a
// credential erases it, and here that would stop every invitation on the next
// send with an AUTH failure the operator never asked for.
//
// ORDER, and it is the identity provider surface's order for that surface's
// reason: the document is validated first; the seal is ADDITIVE, so it may
// precede the row; the CLEAR is destructive, so it follows the committed row.
// A cleared vault entry followed by a failed row write would leave a stored
// user name with no password and every send refused at AUTH.
func (s *Store) Save(ctx context.Context, settings Settings, password *string, author string) error {
	if !s.Ready() {
		return errors.New("emailsettings: the store is not available")
	}
	settings = settings.Trim()
	if err := settings.Validate(); err != nil {
		return err
	}

	clearing := password != nil && strings.TrimSpace(*password) == ""
	sealing := password != nil && strings.TrimSpace(*password) != ""

	// The pair is checked against what this write WOULD leave in place, never
	// against what is there now. A save that adds a user name while clearing
	// the password must be refused before either half lands.
	willHold := sealing
	if password == nil {
		sealed, err := s.PasswordSealed(ctx)
		if err != nil {
			return err
		}
		willHold = sealed
	}
	if settings.Username != "" && !willHold {
		return FieldError{
			Field:  "password",
			Reason: "a user name needs a password: set both, or clear the user name to submit without authentication",
		}
	}
	if settings.Username == "" && willHold {
		return FieldError{
			Field:  "username",
			Reason: "a password needs a user name: set both, or clear the password to submit without authentication",
		}
	}

	if sealing {
		if err := s.vault.StoreAdminHiddenSecret(ctx, SecretName(), *password); err != nil {
			return fmt.Errorf("emailsettings: seal the password: %w", err)
		}
	}
	if err := s.writeRows(ctx, settings, author); err != nil {
		return err
	}
	if clearing {
		if err := s.vault.DeleteAdminHiddenSecret(ctx, SecretName()); err != nil {
			return fmt.Errorf("emailsettings: clear the password: %w", err)
		}
	}
	return nil
}

// writeRows upserts every declared key in ONE transaction.
//
// Every key is written, the empty ones included. The page saves the whole
// document, so a key left out of the statement list would keep its previous
// value while the form showed it blank: the operator would clear a reply-to
// address, see it gone, and it would still be on the next message.
func (s *Store) writeRows(ctx context.Context, settings Settings, author string) error {
	values := map[string]any{
		KeyHost:          settings.Host,
		KeyPort:          settings.Port,
		KeyUsername:      settings.Username,
		KeyTLS:           settings.TLS,
		KeyFrom:          settings.From,
		KeyReplyTo:       settings.ReplyTo,
		KeyPublicBaseURL: settings.PublicBaseURL,
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Sorted, so two administrators saving at once take the row locks in the
	// same order and cannot deadlock against each other — the rule
	// `admin/config_values.go:storeSectionValues` already states.
	keys := append([]string(nil), storedKeys...)
	sort.Strings(keys)
	for _, key := range keys {
		encoded, err := json.Marshal(values[key])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
            INSERT INTO centry.platform_config (section, key, value, updated_at, updated_by)
            VALUES ($1, $2, $3::jsonb, now(), $4)
            ON CONFLICT (section, key)
            DO UPDATE SET value = EXCLUDED.value, updated_at = now(), updated_by = EXCLUDED.updated_by
        `, Section, key, string(encoded), author); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func stringOf(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

// intOf accepts the JSON number a `jsonb` round trip produces (float64) as
// well as a hand-written integer row: `centry.platform_config` is edited by
// migrations and by people, not only by this package.
func intOf(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0
		}
		return int(parsed)
	default:
		return 0
	}
}
