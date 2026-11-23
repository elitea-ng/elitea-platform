# Webhook signing secrets move to the project vault and are write-only (#2963)

**What changed.** An outbound webhook's signing secret is now stored in the
project's secret vault as a hidden secret. The `webhooks` row keeps only a
reference to it (new column `secret_ref`, shared migration
`0265_webhook_secret_reference.sql`), and the `secret` column is left empty.

- `GET /webhooks/prompt_lib/{projectID}` and
  `GET /webhooks/prompt_lib/{projectID}/{webhookID}` no longer return
  `secret`. They return `has_secret: true|false` instead.
- `POST /webhooks/prompt_lib/{projectID}` returns the submitted `secret` in
  its `201` response, once.
- New route `POST /webhooks/prompt_lib/{projectID}/{webhookID}/rotate_secret`
  (same permission as `PUT`): the server generates a new 256-bit secret,
  deletes the previous one and returns the new value once.
- `PUT /webhooks/prompt_lib/{projectID}/{webhookID}`: `secret` is now
  optional. Leaving it out keeps the stored secret (it used to clear it). A
  value replaces it and `""` removes it. The response never includes it.
- A write that stores a secret answers `503` when the project vault cannot be
  written (for example, a vault sealed with a different `SECRETS_MASTER_KEY`).
  It never falls back to storing plain text.
- Deleting a webhook also deletes its secret from the vault.

**Existing secrets.** On start-up, elitea-main moves every secret still
stored in plain text into its project vault, writes the reference and blanks
the column. This runs in Go rather than in the SQL migration, because only the
service holds the key that opens the vault. One row is moved per transaction,
so a row that cannot be moved (it is logged with its webhook id) does not block
the others and is retried on the next start. Until a row is moved, its
deliveries keep being signed with the stored value, so no delivery goes out
unsigned during the upgrade. Receivers keep working: the value does not
change.

**Who is affected.** Scripts that read `secret` from the list or detail
response, and scripts that rotated a secret by sending a new one through
`PUT` and expected it echoed back.

**What to do.** Save a secret when you create the webhook or rotate it; you
cannot read it back afterwards. To rotate, call `rotate_secret` (or keep
sending a new `secret` through `PUT`, which still replaces it). Check
elitea-main's start-up log for `could not move a signing secret into the
project vault` after the upgrade; no other action is needed.
