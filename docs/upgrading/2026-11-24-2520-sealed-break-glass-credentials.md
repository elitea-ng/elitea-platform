# Sealed break-glass credentials: a new table and three operator commands (#2520, ADR-0059 R2.6a)

**What changed.**

- Shared migration `0271_sealed_credentials.sql` creates
  `elitea_auth.sealed_credentials`: one row per sealed break-glass
  credential, with its selector, the Argon2id digest of its verifier (never
  the verifier), its label, its level (`admin` or `super_admin`), the person
  it is bound to, whether it is split, and its issue, expiry (at most 365
  days), use and revocation times. A trigger refuses `DELETE`, `TRUNCATE` and
  any change but the use and the revocation, each written once. It touches
  no existing table.
- `elitea-main` has three new sub-commands of the `break-glass` operator
  command, run in the workload like `break-glass end`:
  - `break-glass issue --user USER [--level admin|super_admin] [--label TEXT] [--split] [--expires WHEN]`
    prints one credential `bg_<selector>.<verifier>` once (or, with
    `--split`, the selector and two halves of the verifier for two
    custodians). The default level is `admin`; the default and maximum
    lifetime 365 days.
  - `break-glass list` prints every credential and its state, never a secret.
  - `break-glass revoke ID [ID...]` revokes credentials; rows are kept.
- Each writes a security audit event in the same transaction:
  `admin.elevation` `credential_issued` and `credential_revoked` with the
  source `operator` and the level, and a `decision` event
  `admin.elevation.credential_list` for the read.
- `golang.org/x/crypto` is now a direct dependency of elitea-main (Argon2id).

**Nothing is created automatically.** The migration creates an empty table.
No credential is issued on upgrade, the chart has no key for one, never runs
a Job that issues one and never stores one in a Secret. The sign-in page
that takes a credential ships in a later release; until then a credential
can be issued, listed and revoked, not used.

**Who is affected.** Operators who want an emergency way in for an identity
provider outage: see the
[break-glass runbook](../../apps/elitea-docs/src/content/docs/deployment/break-glass-runbook.mdx)
for levels, split knowledge and custody.

**Rollback.** The previous release does not read the table and has no
`issue`, `list` or `revoke` sub-command. The table and its rows stay; the
migration is not reverted.
