package secrets

// Programmatic access to the GLOBAL vault's HIDDEN bucket.
//
// # Who this is for
//
// A platform-wide feature that owns a credential on the operator's behalf,
// rather than a person typing one into the Secrets page. The first such feature
// is the pre-built MCP server catalogue (`internal/mcpregistry`, shared
// migration 0094): the operator defines "GitHub Copilot" once with a client
// secret, and every project can then pick it as a ready-made toolkit without
// ever being shown the secret.
//
// # Why HIDDEN and not the regular bucket
//
// `EngineBase.get_all_secrets` merges the global vault's REGULAR secrets into
// the set every project resolves `{{secret.<name>}}` against. A catalogue
// client secret placed there would become readable, as plaintext, by any agent
// in any project that interpolated its name — and the names are derived from
// the catalogue key, so they are guessable by anyone who can see the toolkit.
//
// The hidden bucket is excluded from that merge, which
// `internal/infra/storage/postgres_secret_vault.go` states as the whole purpose
// of `LookupRegular`: the shared-admin fallback "must not expose hidden admin
// secrets to project workloads". So a hidden entry is reachable only by code in
// this service that asks for it by name, which is exactly the access the
// catalogue resolver needs and no more.
//
// # These are not routes
//
// Nothing here is registered on a router. The Secrets page's own six
// administration routes stay the only HTTP surface on this vault, and they are
// gated on `configuration.secrets.secret.*`. A feature that calls these methods
// carries its own gate on its own surface; the catalogue's is `runtime.plugins`,
// the same permission the admin Configuration page it replaces already
// requires.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// StoreAdminHiddenSecret writes one hidden entry into the global vault,
// creating the vault on first use.
//
// The write OVERWRITES an existing entry of the same name. That is deliberate
// and differs from `AdminCreate`, which refuses to overwrite: this is not a
// person creating a named secret that might collide with someone else's, it is
// a feature restating the current value of a credential it owns under a name it
// derived. Refusing here would make an operator's second save of the same
// catalogue entry fail with a collision against its own previous save.
func (h *Handler) StoreAdminHiddenSecret(ctx context.Context, name, value string) error {
	if !validSecretName.MatchString(name) {
		return fmt.Errorf("secrets: %q is not a valid secret name", name)
	}
	return h.mutateAdminVault(ctx, true, func(vault *vaultData) (bool, error) {
		// A name already used by a REGULAR global secret is refused rather
		// than shadowed. The two buckets are read by different lookups, so a
		// name in both would resolve to one value for project interpolation
		// and another for this feature — a divergence nothing would report.
		if _, taken := vault.Secrets[name]; taken {
			return false, fmt.Errorf("secrets: %q is already a global secret", name)
		}
		vault.HiddenSecrets[name] = value
		return true, nil
	})
}

// LookupAdminHiddenSecret reads one hidden entry.
//
// A vault that exists and holds no such name returns ErrSecretNotFound, which
// the caller can distinguish from a vault that would not open. The distinction
// matters: "the operator has not set this credential" and "this service cannot
// read its own vault" call for different messages, and collapsing them is how a
// broken vault gets reported to a user as a missing configuration.
func (h *Handler) LookupAdminHiddenSecret(ctx context.Context, name string) (string, error) {
	vault, err := h.readVaultByID(ctx, adminVaultKey)
	if err != nil {
		return "", err
	}
	if value, ok := vault.HiddenSecrets[name]; ok {
		return value, nil
	}
	return "", ErrSecretNotFound
}

// DeleteAdminHiddenSecret removes one hidden entry.
//
// Deleting a name that is not there succeeds. The caller is a feature cleaning
// up after a definition it is removing, and a missing entry means the cleanup
// is already done; reporting that as an error would make a delete fail for
// having nothing left to do.
func (h *Handler) DeleteAdminHiddenSecret(ctx context.Context, name string) error {
	return h.mutateAdminVault(ctx, false, func(vault *vaultData) (bool, error) {
		if _, present := vault.HiddenSecrets[name]; !present {
			return false, nil
		}
		delete(vault.HiddenSecrets, name)
		return true, nil
	})
}

// AdminHiddenSecretName derives the vault name a feature stores a credential
// under.
//
// The shape is `<feature>__<readable subject>_<digest>__<field>`. Every part is
// lowercased and reduced to `[a-z0-9_]`, because `validSecretName` accepts
// `[A-Za-z0-9_]+` only and a subject that comes from an operator-typed name
// would otherwise produce a name the vault refuses.
//
// # Why the digest is not decoration
//
// The readable reduction is MANY-TO-ONE, and the collisions are reachable, not
// theoretical. Two distinct catalogue keys reduce to one readable form in at
// least two ways:
//
//   - `epam_presales` and `__epam_presales__` differ only in padding, and both
//     are storable keys — `NormalizeCatalogueKey` turns a padded display name
//     into a padded key, because pylon's own normalisation strips whitespace
//     only after the spaces have already become underscores.
//   - `a-b` and `a/b` both reduce to `a_b`, since every character outside the
//     accepted class becomes an underscore.
//
// A collision here is not cosmetic: two catalogue entries would share one vault
// entry, so saving the second would overwrite the first's client secret and
// the first would then authenticate with the wrong credential. The digest of
// the RAW subject makes the derivation injective, so that cannot happen.
//
// The readable part is kept because it is what makes an entry identifiable if
// anyone ever inspects the vault; the digest is what makes it correct.
func AdminHiddenSecretName(feature, subject, field string) string {
	digest := sha256.Sum256([]byte(subject))
	return fmt.Sprintf("%s__%s_%s__%s",
		vaultNamePart(feature),
		vaultNamePart(subject),
		hex.EncodeToString(digest[:4]),
		vaultNamePart(field),
	)
}

// vaultNamePart reduces one component to the characters a vault name may carry.
//
// Runs of unacceptable characters collapse to a single underscore and the
// result is trimmed, which is what keeps the readable part readable. Both of
// those steps lose information, which is precisely why AdminHiddenSecretName
// does not rely on this function alone to tell two subjects apart.
func vaultNamePart(part string) string {
	var builder strings.Builder
	var lastUnderscore bool
	for _, character := range strings.ToLower(part) {
		switch {
		case character >= 'a' && character <= 'z',
			character >= '0' && character <= '9':
			builder.WriteRune(character)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				builder.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(builder.String(), "_")
}
