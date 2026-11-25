package adminaccess

// The sealed break-glass credentials (ADR-0059 decision 5, "Path 2: a sealed
// credential when single sign-on is down"; story R2.6a, issue 2520).
//
// A credential is `bg_<selector>.<verifier>`. The selector is random, not
// secret, and finds the row of elitea_auth.sealed_credentials (shared 0271).
// The verifier is 256 random bits; ONLY ITS ARGON2ID DIGEST IS STORED, in the
// PHC string form that carries its parameters. The verifier appears once, in
// what IssueSealedCredential returns to the operator command, and nowhere
// else: not in the row, not in a log line, not in the security audit event.
//
// The writers run in the caller's transaction and write their security audit
// event on it, under one savepoint with the row, like the elevation ledger:
// credential_issued and credential_revoked are admin.elevation events (R2.3)
// with the source of the step; the list is an operator READ, recorded as a
// decision event (SealedCredentialListAction), because reading the list of
// emergency credentials is itself evidence.
//
// VerifySealedCredential is the check of the sign-in page (R2.7b): an unknown
// selector costs no hash, a known one costs exactly one Argon2id comparison,
// in constant time, and a used, revoked or expired credential is refused
// whatever the verifier. It does not consume: the consume is R2.7b's one
// statement.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/argon2"

	"github.com/elitea-ng/elitea-platform/libs/go/secretstrength"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// SealedCredentialPrefix starts every sealed credential.
const SealedCredentialPrefix = "bg_"

// The sizes of a credential, in random bytes; both are hex in the credential.
const (
	sealedSelectorBytes = 12
	sealedVerifierBytes = 32
)

// MaxSealedCredentialLifetime is the ceiling in code (ADR-0059 decision 5): a
// credential lives at most 365 days. The table's CHECK holds the same bound.
const MaxSealedCredentialLifetime = 365 * 24 * time.Hour

// DefaultSealedCredentialLifetime is the lifetime of a credential issued
// without an expiry: the ceiling.
const DefaultSealedCredentialLifetime = MaxSealedCredentialLifetime

// maxSealedLabelRunes bounds the label, as the table's CHECK does.
const maxSealedLabelRunes = 200

// The Argon2id parameters of a new digest (RFC 9106, section 4, the second
// recommended option: 64 MiB, 3 passes, 4 lanes). They are written into the
// digest, so a later change of these values leaves every stored digest
// verifiable.
const (
	argon2Memory  uint32 = 64 * 1024
	argon2Time    uint32 = 3
	argon2Threads uint8  = 4
	argon2KeyLen  uint32 = 32
	argon2SaltLen        = 16
)

// The bounds a stored digest's parameters must fall in before a comparison
// runs: a row written by hand cannot make one comparison cost a gigabyte.
const (
	maxArgon2Memory  uint32 = 256 * 1024
	maxArgon2Time    uint32 = 10
	maxArgon2Threads uint8  = 16
)

// SealedCredentialListAction is the Action of the decision event the list
// writes: an operator read of the sealed credentials.
const SealedCredentialListAction = "admin.elevation.credential_list"

// The states of a credential at a moment.
const (
	SealedStateActive  = "active"
	SealedStateUsed    = "used"
	SealedStateRevoked = "revoked"
	SealedStateExpired = "expired"
)

var (
	// ErrSealedCredentialInvalid wraps every refusal of an issue before
	// anything is written.
	ErrSealedCredentialInvalid = errors.New("sealed credential refused")
	// ErrSealedCredentialPerson: the person is not an unsuspended account.
	ErrSealedCredentialPerson = errors.New("sealed credential refused: the person is not an active account")
	// ErrSealedCredentialReserved: a bound issue names the reserved
	// break-glass account. Its credentials are unbound ones (SealedIssue.
	// Unbound), with a ticket and a custodian label.
	ErrSealedCredentialReserved = errors.New("sealed credential refused: the reserved break-glass account takes " +
		"unbound credentials only")
	// ErrSealedCredentialNotFound: no credential has this id.
	ErrSealedCredentialNotFound = errors.New("sealed credential not found")
	// ErrSealedCredentialRefused is the one answer of VerifySealedCredential
	// to anything but a usable credential with its verifier. The wrapped
	// cause (SealedRefusal) is for the server log, never for the caller.
	ErrSealedCredentialRefused = errors.New("sealed credential refused")
)

// SealedRefusal is why VerifySealedCredential refused, for the server log.
type SealedRefusal string

// The causes of a refusal.
const (
	SealedRefusalMalformed SealedRefusal = "malformed"
	SealedRefusalUnknown   SealedRefusal = "unknown_selector"
	SealedRefusalMismatch  SealedRefusal = "verifier_mismatch"
	SealedRefusalUsed      SealedRefusal = "used"
	SealedRefusalRevoked   SealedRefusal = "revoked"
	SealedRefusalExpired   SealedRefusal = "expired"
)

// sealedRefusalError carries the cause of a refusal.
type sealedRefusalError struct{ cause SealedRefusal }

func (e sealedRefusalError) Error() string {
	return ErrSealedCredentialRefused.Error() + ": " + string(e.cause)
}

func (e sealedRefusalError) Unwrap() error { return ErrSealedCredentialRefused }

// SealedRefusalOf answers the cause of a refusal of VerifySealedCredential,
// "" for any other error.
func SealedRefusalOf(err error) SealedRefusal {
	var refusal sealedRefusalError
	if errors.As(err, &refusal) {
		return refusal.cause
	}
	return ""
}

// SealedCredential is one row of elitea_auth.sealed_credentials. It has no
// field for the verifier: the row has none. A zero time is a NULL column.
type SealedCredential struct {
	ID        uuid.UUID
	Selector  string
	Label     string
	Level     string
	UserID    int64
	Split     bool
	IssuedAt  time.Time
	ExpiresAt time.Time
	UsedAt    time.Time
	RevokedAt time.Time
}

// State is the credential's state at now: revoked and used are final, and an
// unused, unrevoked credential past its expiry is expired.
func (c SealedCredential) State(now time.Time) string {
	switch {
	case !c.RevokedAt.IsZero():
		return SealedStateRevoked
	case !c.UsedAt.IsZero():
		return SealedStateUsed
	case !now.Before(c.ExpiresAt):
		return SealedStateExpired
	}
	return SealedStateActive
}

// SealedIssue is what issues a credential.
type SealedIssue struct {
	// UserID is the person the credential is bound to. 0 with Unbound: the
	// issue binds it to the reserved account.
	UserID int64
	// Unbound issues the credential for the reserved break-glass account
	// (ReservedBreakGlassEmail; story R2.6b), creating the account when it is
	// absent: an envelope in a safe cannot name the person who will open it.
	// It requires Ticket and Label, and UserID 0.
	Unbound bool
	// Ticket is the change or incident ticket the issue is made under, an
	// identifier (SealedTicketPattern); required with Unbound, optional
	// otherwise. The credential_issued event carries it as the reason code
	// `ticket:<lower-cased ticket>`.
	Ticket string
	// Level is the level of the elevation the credential starts: admin
	// (the default when empty) or super_admin.
	Level string
	// Label is what the custodians call the credential; optional.
	Label string
	// Split prints the verifier as two halves for two custodians.
	Split bool
	// At is the issue time, from the caller's clock. Required.
	At time.Time
	// Lifetime is the time to the expiry, at most
	// MaxSealedCredentialLifetime; 0 takes the default.
	Lifetime time.Duration
	// Source is the source of the credential_issued event; "" is operator.
	Source string
	// ActorUserID is who issued it; 0 for an operator command.
	ActorUserID int64
}

// IssuedSealedCredential is an issued credential with its secret, for the
// one output the operator sees. Nothing else ever holds Verifier.
type IssuedSealedCredential struct {
	SealedCredential
	// Verifier is the secret half of the credential, in hex.
	Verifier string
	// ReservedAccountCreated: the issue was unbound and created the reserved
	// account (the first `issue --unbound`).
	ReservedAccountCreated bool
}

// Credential is the whole credential, `bg_<selector>.<verifier>`.
func (c IssuedSealedCredential) Credential() string {
	return FormatSealedCredential(c.Selector, c.Verifier)
}

// Halves is the verifier as two halves for two custodians: they concatenate
// to the verifier.
func (c IssuedSealedCredential) Halves() (string, string) {
	half := len(c.Verifier) / 2
	return c.Verifier[:half], c.Verifier[half:]
}

// FormatSealedCredential joins a selector and a verifier into a credential.
func FormatSealedCredential(selector, verifier string) string {
	return SealedCredentialPrefix + selector + "." + verifier
}

var (
	sealedSelectorPattern = regexp.MustCompile(`^[0-9a-f]{24}$`)
	sealedVerifierPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ParseSealedCredential splits a credential into its selector and verifier.
// Surrounding white space is ignored; the hex is case-insensitive, so a
// credential read from paper may be typed in capitals.
func ParseSealedCredential(credential string) (selector, verifier string, err error) {
	value := strings.ToLower(strings.TrimSpace(credential))
	rest, ok := strings.CutPrefix(value, SealedCredentialPrefix)
	if !ok {
		return "", "", sealedRefusalError{SealedRefusalMalformed}
	}
	selector, verifier, ok = strings.Cut(rest, ".")
	if !ok || !sealedSelectorPattern.MatchString(selector) || !sealedVerifierPattern.MatchString(verifier) {
		return "", "", sealedRefusalError{SealedRefusalMalformed}
	}
	return selector, verifier, nil
}

// SealedTicketPattern is the shape of a ticket: an identifier such as
// `INC-4711` or `CHG0012345`, never free text. It is recorded lower-cased in
// the event, as a reason code.
var SealedTicketPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// The reason codes the credential_issued event of an unbound issue carries.
const (
	SealedReasonUnbound      = "credential_unbound"
	SealedReasonTicketPrefix = "ticket:"
)

// Validate checks an issue without touching the database.
func (s SealedIssue) Validate() error {
	switch {
	case s.Unbound && s.UserID != 0:
		return fmt.Errorf("%w: an unbound credential names no person", ErrSealedCredentialInvalid)
	case s.Unbound && strings.TrimSpace(s.Ticket) == "":
		return fmt.Errorf("%w: an unbound credential needs the ticket it is issued under", ErrSealedCredentialInvalid)
	case s.Unbound && strings.TrimSpace(s.Label) == "":
		return fmt.Errorf("%w: an unbound credential needs a label naming its custodian", ErrSealedCredentialInvalid)
	case !s.Unbound && s.UserID <= 0:
		return fmt.Errorf("%w: a credential is bound to a person", ErrSealedCredentialInvalid)
	case s.Ticket != "" && !SealedTicketPattern.MatchString(s.Ticket):
		return fmt.Errorf("%w: the ticket %q is not an identifier (letters, digits, '.', '_', ':' and '-', at most 64)",
			ErrSealedCredentialInvalid, s.Ticket)
	}
	switch s.level() {
	case ElevationLevelAdmin, ElevationLevelSuperAdmin:
	default:
		return fmt.Errorf("%w: unknown level %q", ErrSealedCredentialInvalid, s.Level)
	}
	if s.At.IsZero() {
		return fmt.Errorf("%w: no time given", ErrSealedCredentialInvalid)
	}
	if lifetime := s.lifetime(); lifetime <= 0 || lifetime > MaxSealedCredentialLifetime {
		return fmt.Errorf("%w: the lifetime %s is outside (0, 365 days]", ErrSealedCredentialInvalid, lifetime)
	}
	if label := s.Label; len([]rune(label)) > maxSealedLabelRunes || strings.IndexFunc(label, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: the label is longer than %d characters or holds a control character",
			ErrSealedCredentialInvalid, maxSealedLabelRunes)
	}
	if s.ActorUserID < 0 {
		return fmt.Errorf("%w: a negative actor", ErrSealedCredentialInvalid)
	}
	if s.Source != "" && !knownElevationSource(s.Source) {
		return fmt.Errorf("%w: unknown event source %q", ErrSealedCredentialInvalid, s.Source)
	}
	return nil
}

func (s SealedIssue) level() string {
	if s.Level == "" {
		return ElevationLevelAdmin
	}
	return s.Level
}

func (s SealedIssue) lifetime() time.Duration {
	if s.Lifetime == 0 {
		return DefaultSealedCredentialLifetime
	}
	return s.Lifetime
}

func sourceOrOperator(source string) string {
	if source == "" {
		return securityaudit.ElevationSourceOperator
	}
	return source
}

// sealedColumns is the SELECT list scanSealed reads. It never names
// verifier_digest: nothing that reads a credential for display holds it.
const sealedColumns = `id, selector, label, level, user_id, split, issued_at, expires_at, used_at, revoked_at`

func scanSealed(row pgx.Row) (SealedCredential, error) {
	var c SealedCredential
	var used, revoked *time.Time
	if err := row.Scan(&c.ID, &c.Selector, &c.Label, &c.Level, &c.UserID, &c.Split,
		&c.IssuedAt, &c.ExpiresAt, &used, &revoked); err != nil {
		return SealedCredential{}, err
	}
	if used != nil {
		c.UsedAt = *used
	}
	if revoked != nil {
		c.RevokedAt = *revoked
	}
	return c, nil
}

// IssueSealedCredential writes one credential bound to a person, with its
// credential_issued event, in the caller's transaction, and returns it with
// its verifier. The row holds the Argon2id digest of the verifier only.
//
// It refuses, before it writes, an invalid issue (Validate) and a person who
// is not an unsuspended account (ErrSealedCredentialPerson). The row and its
// event are one savepoint: a failed append leaves no row, even when the
// caller commits after the error.
func IssueSealedCredential(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, issue SealedIssue) (IssuedSealedCredential, error) {
	if err := issue.Validate(); err != nil {
		return IssuedSealedCredential{}, err
	}
	if appender == nil {
		return IssuedSealedCredential{}, ErrElevationNoAppender
	}
	if tx == nil {
		return IssuedSealedCredential{}, errors.New("issue sealed credential: no transaction")
	}
	if !issue.Unbound {
		var person, reserved bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM public.auth_core__user WHERE id = $1 AND suspended = false),
       EXISTS (SELECT 1 FROM public.auth_core__user WHERE id = $1 AND lower(email) = $2)`,
			issue.UserID, ReservedBreakGlassEmail).Scan(&person, &reserved); err != nil {
			return IssuedSealedCredential{}, fmt.Errorf("issue sealed credential: read the person: %w", err)
		}
		if reserved {
			// A credential of the reserved account is an unbound one, and
			// needs its ticket and its custodian label.
			return IssuedSealedCredential{}, ErrSealedCredentialReserved
		}
		if !person {
			return IssuedSealedCredential{}, ErrSealedCredentialPerson
		}
	}
	selector, err := randomHex(sealedSelectorBytes)
	if err != nil {
		return IssuedSealedCredential{}, err
	}
	verifier, err := randomHex(sealedVerifierBytes)
	if err != nil {
		return IssuedSealedCredential{}, err
	}
	// The same floor every signing secret meets (libs/go/secretstrength): a
	// broken random source that answers a constant cannot issue a credential.
	if err := secretstrength.Check("the sealed credential verifier", verifier); err != nil {
		return IssuedSealedCredential{}, fmt.Errorf("issue sealed credential: %w", err)
	}
	digest, err := sealedDigest(verifier)
	if err != nil {
		return IssuedSealedCredential{}, err
	}
	at := issue.At.UTC()
	var credential SealedCredential
	created := false
	err = withSavepoint(ctx, tx, func(sp pgx.Tx) error {
		userID := issue.UserID
		if issue.Unbound {
			// In the savepoint: an issue that fails creates no account.
			var err error
			if userID, created, err = EnsureReservedBreakGlassAccount(ctx, sp); err != nil {
				return err
			}
		}
		var err error
		credential, err = scanSealed(sp.QueryRow(ctx, `
INSERT INTO elitea_auth.sealed_credentials
    (selector, verifier_digest, label, level, user_id, split, issued_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING `+sealedColumns,
			selector, digest, strings.TrimSpace(issue.Label), issue.level(), userID, issue.Split,
			at, at.Add(issue.lifetime())))
		if err != nil {
			return fmt.Errorf("issue sealed credential: %w", err)
		}
		var reasons []string
		if issue.Unbound {
			reasons = append(reasons, SealedReasonUnbound)
		}
		if issue.Ticket != "" {
			reasons = append(reasons, SealedReasonTicketPrefix+strings.ToLower(issue.Ticket))
		}
		return appendSealedEvent(ctx, sp, appender, securityaudit.ElevationCredentialIssued,
			sourceOrOperator(issue.Source), issue.ActorUserID, credential, reasons...)
	})
	if err != nil {
		return IssuedSealedCredential{}, err
	}
	return IssuedSealedCredential{SealedCredential: credential, Verifier: verifier, ReservedAccountCreated: created}, nil
}

// appendSealedEvent writes the admin.elevation event of a credential step:
// its id, its person and its level, on the sealed path. Never the selector,
// the verifier or the digest. extra are further reason codes (an unbound
// issue's marker and ticket).
func appendSealedEvent(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender,
	action securityaudit.ElevationAction, source string, actor int64, credential SealedCredential, extra ...string,
) error {
	reasons := []string{"credential_level:" + credential.Level}
	if credential.Split {
		reasons = append(reasons, "credential_split")
	}
	reasons = append(reasons, extra...)
	return AppendElevationEvent(ctx, tx, appender, ElevationEvent{
		Action:       action,
		Source:       source,
		Elevation:    ElevationRecord{UserID: credential.UserID, Level: credential.Level, Path: ElevationPathSealed},
		ActorUserID:  actor,
		CredentialID: credential.ID.String(),
		Reasons:      reasons,
	})
}

// SealedList is what lists the credentials.
type SealedList struct {
	// Source is the source recorded on the read; "" is operator.
	Source string
	// ActorUserID is who read; 0 for an operator command.
	ActorUserID int64
}

// ListSealedCredentials answers every credential, active or not, newest
// first, and records the read: one decision event with the action
// SealedCredentialListAction, the source and the number of rows, on tx. The
// read and its record commit together.
func ListSealedCredentials(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, list SealedList) ([]SealedCredential, error) {
	source := sourceOrOperator(list.Source)
	if !knownElevationSource(source) {
		return nil, fmt.Errorf("%w: unknown event source %q", ErrSealedCredentialInvalid, source)
	}
	if appender == nil {
		return nil, ErrElevationNoAppender
	}
	if tx == nil {
		return nil, errors.New("list sealed credentials: no transaction")
	}
	rows, err := tx.Query(ctx, `SELECT `+sealedColumns+` FROM elitea_auth.sealed_credentials ORDER BY issued_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("list sealed credentials: %w", err)
	}
	credentials, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SealedCredential, error) { return scanSealed(row) })
	if err != nil {
		return nil, fmt.Errorf("list sealed credentials: %w", err)
	}
	principal := ""
	if list.ActorUserID > 0 {
		principal = "user:" + strconv.FormatInt(list.ActorUserID, 10)
	}
	event := securityaudit.SecurityEvent{
		Type:         securityaudit.TypeDecision,
		Action:       SealedCredentialListAction,
		Effect:       securityaudit.EffectAllow,
		Reasons:      []string{"source:" + source, "credentials:" + strconv.Itoa(len(credentials))},
		PrincipalRef: principal,
		ResourceType: "sealed_credential",
	}
	if err := appender.Append(ctx, tx, event); err != nil {
		return nil, fmt.Errorf("list sealed credentials: record the read: %w", err)
	}
	return credentials, nil
}

// SealedRevoke is what revokes a credential.
type SealedRevoke struct {
	ID uuid.UUID
	// At is the revocation time, from the caller's clock. Required.
	At time.Time
	// Source is the source of the credential_revoked event; "" is operator.
	Source string
	// ActorUserID is who revoked it; 0 for an operator command.
	ActorUserID int64
}

// RevokeSealedCredential sets the revoked time of an unused, unrevoked
// credential, with its credential_revoked event, in the caller's
// transaction. The row is kept. It reports whether it wrote anything: a
// credential already revoked or used changes nothing, writes no event, and
// is returned as it is, with false. An unknown id is ErrSealedCredentialNotFound.
func RevokeSealedCredential(ctx context.Context, tx pgx.Tx, appender securityaudit.Appender, revoke SealedRevoke) (SealedCredential, bool, error) {
	source := sourceOrOperator(revoke.Source)
	switch {
	case revoke.At.IsZero():
		return SealedCredential{}, false, fmt.Errorf("%w: no time given", ErrSealedCredentialInvalid)
	case !knownElevationSource(source):
		return SealedCredential{}, false, fmt.Errorf("%w: unknown event source %q", ErrSealedCredentialInvalid, source)
	case appender == nil:
		return SealedCredential{}, false, ErrElevationNoAppender
	case tx == nil:
		return SealedCredential{}, false, errors.New("revoke sealed credential: no transaction")
	}
	var credential SealedCredential
	err := withSavepoint(ctx, tx, func(sp pgx.Tx) error {
		var err error
		credential, err = scanSealed(sp.QueryRow(ctx, `
UPDATE elitea_auth.sealed_credentials
SET revoked_at = GREATEST($2::timestamptz, issued_at)
WHERE id = $1 AND revoked_at IS NULL AND used_at IS NULL
RETURNING `+sealedColumns, revoke.ID, revoke.At.UTC()))
		if err != nil {
			return err
		}
		return appendSealedEvent(ctx, sp, appender, securityaudit.ElevationCredentialRevoked, source, revoke.ActorUserID, credential)
	})
	if err == nil {
		return credential, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return SealedCredential{}, false, fmt.Errorf("revoke sealed credential: %w", err)
	}
	current, err := ReadSealedCredential(ctx, tx, revoke.ID)
	if err != nil {
		return SealedCredential{}, false, err
	}
	return current, false, nil
}

// ReadSealedCredential reads one credential by its id.
func ReadSealedCredential(ctx context.Context, q Querier, id uuid.UUID) (SealedCredential, error) {
	credential, err := scanSealed(q.QueryRow(ctx, `SELECT `+sealedColumns+` FROM elitea_auth.sealed_credentials WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return SealedCredential{}, ErrSealedCredentialNotFound
	}
	if err != nil {
		return SealedCredential{}, fmt.Errorf("read sealed credential: %w", err)
	}
	return credential, nil
}

// VerifySealedCredential answers the credential a presented value names when
// it is usable at at: its verifier matches, and it is not used, revoked or
// expired. Every refusal is ErrSealedCredentialRefused, whatever failed; the
// cause is SealedRefusalOf(err), for the server log only. Any other error is
// the database's.
//
// An unknown selector costs no hash. A known selector costs exactly one
// Argon2id comparison, made before the state is looked at, so a used or
// revoked credential takes as long to refuse as a wrong verifier. It writes
// nothing: the consume is the sign-in's one statement (R2.7b).
func VerifySealedCredential(ctx context.Context, q Querier, presented string, at time.Time) (SealedCredential, error) {
	selector, verifier, err := ParseSealedCredential(presented)
	if err != nil {
		return SealedCredential{}, err
	}
	var credential SealedCredential
	var digest string
	var used, revoked *time.Time
	err = q.QueryRow(ctx, `
SELECT id, selector, label, level, user_id, split, issued_at, expires_at, used_at, revoked_at, verifier_digest
FROM elitea_auth.sealed_credentials WHERE selector = $1`, selector).Scan(
		&credential.ID, &credential.Selector, &credential.Label, &credential.Level, &credential.UserID,
		&credential.Split, &credential.IssuedAt, &credential.ExpiresAt, &used, &revoked, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return SealedCredential{}, sealedRefusalError{SealedRefusalUnknown}
	}
	if err != nil {
		return SealedCredential{}, fmt.Errorf("verify sealed credential: %w", err)
	}
	if used != nil {
		credential.UsedAt = *used
	}
	if revoked != nil {
		credential.RevokedAt = *revoked
	}
	match, err := sealedDigestMatches(digest, verifier)
	if err != nil {
		// A stored digest this code cannot read is refused like a mismatch;
		// the cause is in the error for the log.
		return SealedCredential{}, fmt.Errorf("%w: %w", sealedRefusalError{SealedRefusalMismatch}, err)
	}
	if !match {
		return SealedCredential{}, sealedRefusalError{SealedRefusalMismatch}
	}
	switch credential.State(at) {
	case SealedStateRevoked:
		return SealedCredential{}, sealedRefusalError{SealedRefusalRevoked}
	case SealedStateUsed:
		return SealedCredential{}, sealedRefusalError{SealedRefusalUsed}
	case SealedStateExpired:
		return SealedCredential{}, sealedRefusalError{SealedRefusalExpired}
	}
	return credential, nil
}

func randomHex(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("sealed credential: read random bytes: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

// sealedDigest is the Argon2id digest of a verifier, in PHC string form.
func sealedDigest(verifier string) (string, error) {
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("sealed credential: read a salt: %w", err)
	}
	key := argon2.IDKey([]byte(verifier), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// sealedDigestMatches compares a verifier with a PHC Argon2id digest, in
// constant time. Parameters outside the bounds are an error, before any hash.
func sealedDigestMatches(digest, verifier string) (bool, error) {
	parts := strings.Split(digest, "$")
	// "", "argon2id", "v=19", "m=…,t=…,p=…", salt, hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return false, errors.New("the stored digest is not an Argon2id digest of this version")
	}
	var memory, passes uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &passes, &threads); err != nil {
		return false, fmt.Errorf("the stored digest's parameters: %w", err)
	}
	if memory == 0 || memory > maxArgon2Memory || passes == 0 || passes > maxArgon2Time || threads == 0 || threads > maxArgon2Threads {
		return false, errors.New("the stored digest's parameters are outside the accepted bounds")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return false, errors.New("the stored digest's salt is unreadable")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) < 16 || len(want) > 64 {
		return false, errors.New("the stored digest's hash is unreadable")
	}
	got := argon2.IDKey([]byte(verifier), salt, passes, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
