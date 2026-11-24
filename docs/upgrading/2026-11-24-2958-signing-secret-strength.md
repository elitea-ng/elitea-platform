# Signing secrets: short, placeholder and published values refused at start-up (#2958)

**What changed.** elitea-main, the LLM gateway and the sub-application hosts
(DeepWiki, Inventory) check their signing secrets before they derive a key
from them. A secret is refused when it is blank, shorter than 32 bytes, a
well-known placeholder (`changeme`, `secret`, `password`, `elitea` and
similar), a value with fewer than 8 different characters (for example the
all-`A` example key), or a value that was once committed to this repository. The error
names the variable, never the value. The checked variables:

- elitea-main: `APPLICATION_SECRET_KEY`, `GATEWAY_IDENTITY_SECRET`,
  `ELITEA_DEEPWIKI_IDENTITY_SECRET` and `ELITEA_INVENTORY_IDENTITY_SECRET`.
  An unset `APPLICATION_SECRET_KEY` still starts with a warning, as before.
  An identity secret is required when its provider is enabled
  (`ELITEA_DEEPWIKI_ENABLED`, `ELITEA_INVENTORY_ENABLED`).
- The LLM gateway: `GATEWAY_IDENTITY_SECRET` (required, see the #2959 note)
  and `GATEWAY_HOP_SECRET` (when set).
- A sub-application host: `<PREFIX>IDENTITY_SECRET`.

`SECRETS_MASTER_KEY`: elitea-main and the gateway refuse the vault master key
that was once committed to `deploy/docker-compose.staging.yml`. That key is
burned.

The development opt-out `ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS=true` is refused
when `OIDC_ISSUER_URL` or `ELITEA_AUTH_CONFIG_FILE` is set. Alone it still
starts, with the warning.

The Helm chart refuses the same values while it renders: a weak value of one
of these variables in `main.env`, `llmGateway.env`, `deepwiki.env` or
`inventory.env`, and the opt-out next to `main.fileConfig.authConfig.enabled`,
`main.env.OIDC_ISSUER_URL` or `main.env.ELITEA_AUTH_CONFIG_FILE`.

The compose files carry no literal secret any more. `deploy/docker-compose.yml`
and the staging overlay require `APPLICATION_SECRET_KEY` from your shell. The
stack scripts (`deploy/scripts/standalone-stack.sh`,
`apps/elitea-web/scripts/e2e-stack.sh`, `deploy/kind/kind-stack.sh`) generate
every signing secret and a vault master key once per stack and keep them
(`deploy/.stack-secrets/<project>.env`, ignored by git, or a Secret on kind).

**Who is affected.** A deployment that runs with a short or placeholder
signing secret, with the burned master key, or with the opt-out next to a
sign-in configuration. Such a deployment does not start after the upgrade.

**What to do.**

1. Before you upgrade, check each variable above. Generate a new value with
   `openssl rand -base64 32` where one is short or a placeholder, and keep it
   in a Secret.
2. Plan the `APPLICATION_SECRET_KEY` rotation. Personal access tokens signed
   with the old key stop working, and their owners must make new ones.
   Rotate an identity secret on both sides at the same time (elitea-main and
   the gateway or the provider host), or the calls between them fail.
3. If you ran with the burned master key, make a new Fernet key and convert
   the vault rows to it with `deploy/scripts/rewrap-centry-vault.py`
   (`--from-key` the old key, `--to-key` the new one). Do it on a copy first.
4. If you ran with `ELITEA_DEV_ALLOW_UNWRAPPED_SECRETS=true` next to a sign-in
   configuration, make a master key, wrap the rows with
   `deploy/scripts/rewrap-centry-vault.py --to-key`, then remove the opt-out.
5. A local stack made before this change: run `down -v` once, so the next
   `up` starts with the generated keys.
