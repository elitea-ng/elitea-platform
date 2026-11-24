package auth

// The callback-token minter, without a database.
//
// The parts worth pinning are the ones a Postgres test would not reach anyway:
// the lifetime floor that the signed expiry's minute precision imposes, the
// rounding direction, the project binding always being asked for, and the
// signer-beats-raw-key precedence. tokens_postgres_integration_test.go covers
// what the token row does; this covers what is asked of it.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type fakeTokenRepository struct {
	created []tokenCreateInput
	deleted []string
	record  tokenRecord
	err     error
}

func (f *fakeTokenRepository) List(context.Context, int64) ([]tokenRecord, error) {
	return nil, nil
}

func (f *fakeTokenRepository) GetOwned(context.Context, int64, string) (tokenRecord, error) {
	return tokenRecord{}, errTokenNotFound
}

func (f *fakeTokenRepository) Create(_ context.Context, input tokenCreateInput) (tokenRecord, error) {
	f.created = append(f.created, input)
	if f.err != nil {
		return tokenRecord{}, f.err
	}
	record := f.record
	record.Expires = input.Expires
	return record, nil
}

func (f *fakeTokenRepository) DeleteOwned(_ context.Context, _ int64, tokenUUID string) error {
	f.deleted = append(f.deleted, tokenUUID)
	return nil
}

func (f *fakeTokenRepository) SetAdminAccess(context.Context, tokenAdminAccessUpdate) (tokenRecord, error) {
	return tokenRecord{}, errTokenNotFound
}

func minterOver(repository tokenRepository, signer TokenSigner, key []byte) *CallbackTokenMinter {
	return &CallbackTokenMinter{tokens: repository, signer: signer, signingKey: key}
}

func uuidPointer(value string) *string { return &value }

// A minted token is ALWAYS bound and ALWAYS expiring. Neither is a default
// that a caller could omit: an unbound token reaches every project its owner
// can, and one without an expiry is the legacy system PAT this replaces.
func TestEveryMintIsBoundAndExpiring(t *testing.T) {
	repository := &fakeTokenRepository{record: tokenRecord{ID: 5, UUID: uuidPointer("u-1"), UserID: 11}}
	minter := minterOver(repository, nil, []byte("signing-key"))

	token, err := minter.Mint(context.Background(), 11, 7, "deepwiki callback", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(repository.created) != 1 {
		t.Fatalf("created %d rows", len(repository.created))
	}
	input := repository.created[0]
	if input.ProjectID == nil || *input.ProjectID != 7 {
		t.Fatalf("project binding %v", input.ProjectID)
	}
	if input.Expires == nil {
		t.Fatal("a callback token was minted with no expiry")
	}
	if input.Name == nil || *input.Name != "deepwiki callback" {
		t.Fatalf("name %v — an operator reading the token list cannot tell what asked for it", input.Name)
	}
	if token.Bearer == "" || token.UUID != "u-1" {
		t.Fatalf("token %+v", token)
	}
}

// The signed expiry is formatted at MINUTE precision, so a sub-minute
// lifetime cannot be expressed: it would be signed as expiring at the start of
// the containing minute, which is already past for most of that minute. The
// floor makes that a refusal rather than an intermittently dead credential.
func TestASubMinuteLifetimeIsRefused(t *testing.T) {
	repository := &fakeTokenRepository{record: tokenRecord{UUID: uuidPointer("u-1")}}
	minter := minterOver(repository, nil, []byte("signing-key"))

	for _, lifetime := range []time.Duration{0, -time.Hour, 30 * time.Second, 59 * time.Second} {
		if _, err := minter.Mint(context.Background(), 11, 7, "n", lifetime); !errors.Is(
			err, ErrCallbackTokenRefused) {
			t.Fatalf("lifetime %s was accepted: %v", lifetime, err)
		}
	}
	if len(repository.created) != 0 {
		t.Fatal("a refused lifetime still created a token row")
	}
}

// Rounding is UP. Truncating would sign an expiry EARLIER than the lifetime
// asked for — up to a minute early — and the shortfall is invisible until a
// generation runs past it.
func TestTheExpiryIsRoundedUpNotDown(t *testing.T) {
	repository := &fakeTokenRepository{record: tokenRecord{UUID: uuidPointer("u-1")}}
	minter := minterOver(repository, nil, []byte("signing-key"))

	before := time.Now().UTC()
	token, err := minter.Mint(context.Background(), 11, 7, "n", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if token.Expires.Before(before.Add(5 * time.Minute)) {
		t.Fatalf("expiry %s is earlier than the %s asked for from %s",
			token.Expires, 5*time.Minute, before)
	}
	if token.Expires.Truncate(time.Minute) != token.Expires {
		t.Fatalf("expiry %s is not on a minute boundary, so the signature will not carry it",
			token.Expires)
	}
}

// A token that names no row authenticates as nobody. Better to fail than to
// return a string that looks like a credential.
func TestARowWithNoUUIDIsNotSignedIntoABearer(t *testing.T) {
	repository := &fakeTokenRepository{record: tokenRecord{ID: 5, UUID: nil}}
	minter := minterOver(repository, nil, []byte("signing-key"))

	if _, err := minter.Mint(context.Background(), 11, 7, "n", time.Hour); !errors.Is(
		err, ErrCallbackTokenRefused) {
		t.Fatalf("a uuid-less row produced a bearer: %v", err)
	}
}

// A refused project binding is a refused mint, and the repository's own
// transaction is what makes that true — the minter only has to not swallow it.
func TestANonMemberGetsNoToken(t *testing.T) {
	repository := &fakeTokenRepository{
		record: tokenRecord{UUID: uuidPointer("u-1")},
		err:    errTokenProjectForbidden,
	}
	minter := minterOver(repository, nil, []byte("signing-key"))

	_, err := minter.Mint(context.Background(), 11, 7, "n", time.Hour)
	if !errors.Is(err, ErrCallbackTokenRefused) {
		t.Fatalf("a non-member was issued a project-bound token: %v", err)
	}
}

type recordingSigner struct{ calls int }

func (s *recordingSigner) SignPAT(*string, *time.Time) (string, error) {
	s.calls++
	return "signed-by-the-graph", nil
}

// The signer wins over the raw key when both are present. This is not a
// preference: a deployment with an authentication configuration file validates
// with the bytes of credentials.pat_signing_key_file, and signing with
// APPLICATION_SECRET_KEY there produces a token rejected on first use — the
// defect signBaselineToken's own header records.
func TestTheSignerBeatsTheRawKey(t *testing.T) {
	signer := &recordingSigner{}
	repository := &fakeTokenRepository{record: tokenRecord{UUID: uuidPointer("u-1")}}
	minter := minterOver(repository, signer, []byte("the-wrong-key"))

	token, err := minter.Mint(context.Background(), 11, 7, "n", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if signer.calls != 1 || token.Bearer != "signed-by-the-graph" {
		t.Fatalf("signer calls=%d bearer=%q", signer.calls, token.Bearer)
	}
}

// With no signer, the raw key signs — and the result is a bearer the platform's
// own validator can read back, not an opaque string.
func TestTheRawKeyProducesAReadableBearer(t *testing.T) {
	repository := &fakeTokenRepository{record: tokenRecord{UUID: uuidPointer("u-42")}}
	minter := minterOver(repository, nil, []byte("signing-key"))

	token, err := minter.Mint(context.Background(), 11, 7, "n", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.Parse(token.Bearer, func(*jwt.Token) (any, error) {
		return []byte("signing-key"), nil
	})
	if err != nil || !parsed.Valid {
		t.Fatalf("the bearer does not verify under the key that signed it: %v", err)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok || claims["uuid"] != "u-42" {
		t.Fatalf("claims %v do not name the token row", parsed.Claims)
	}
}

// Composition refuses a minter that would issue tokens nothing can validate.
func TestAMinterWithNoWayToSignIsRefused(t *testing.T) {
	if _, err := NewCallbackTokenMinter(nil, nil, []byte("key")); !errors.Is(
		err, ErrCallbackTokenRefused) {
		t.Fatalf("a minter with no pool was accepted: %v", err)
	}
}

func TestRevokeDeletesTheRow(t *testing.T) {
	repository := &fakeTokenRepository{record: tokenRecord{UUID: uuidPointer("u-1")}}
	minter := minterOver(repository, nil, []byte("signing-key"))

	if err := minter.Revoke(context.Background(), 11, "u-1"); err != nil {
		t.Fatal(err)
	}
	if len(repository.deleted) != 1 || repository.deleted[0] != "u-1" {
		t.Fatalf("deleted %v", repository.deleted)
	}
	// A revoke with nothing to revoke must not reach the database.
	if err := minter.Revoke(context.Background(), 11, ""); !errors.Is(
		err, ErrCallbackTokenRefused) {
		t.Fatalf("an empty uuid reached the store: %v", err)
	}
	if len(repository.deleted) != 1 {
		t.Fatalf("deleted %v", repository.deleted)
	}
}

// The minted name reaches the token list an operator reads. It must say what
// asked for the token, since nothing else in the row will.
func TestTheNameSaysWhatAskedForIt(t *testing.T) {
	repository := &fakeTokenRepository{record: tokenRecord{UUID: uuidPointer("u-1")}}
	minter := minterOver(repository, nil, []byte("signing-key"))

	if _, err := minter.Mint(context.Background(), 11, 7, "deepwiki callback (project 7)", time.Hour); err != nil {
		t.Fatal(err)
	}
	name := repository.created[0].Name
	if name == nil || !strings.Contains(*name, "deepwiki") {
		t.Fatalf("name %v", name)
	}
}
