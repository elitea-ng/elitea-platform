# Identity secrets are required, and outbound webhooks are always signed (#2959)

**What changed.**

- **The LLM gateway** refuses to start without `GATEWAY_IDENTITY_SECRET`,
  whatever else is configured (before, only with a database or NATS). With an
  empty secret its verifier refuses every identity; there is no path that
  skips the check. The health and metrics endpoints are not affected.
- **A sub-application host** (DeepWiki, Inventory) refuses to start without
  its `<PREFIX>IDENTITY_SECRET`, and refuses every call when it has none.
  `ELITEA_DEV_INSECURE_IDENTITY=true` lets a host start without the secret on
  a development machine. The host refuses that flag next to `OIDC_ISSUER_URL`
  or `ELITEA_AUTH_CONFIG_FILE`.
- **Outbound webhooks.** Creating a webhook without a secret now generates a
  random 32-byte secret, returned once in the create response, as a submitted
  secret is. An update with no secret, or an empty one, keeps the stored
  secret, and a secret cannot be removed. A delivery is never sent unsigned:
  a webhook with no secret records a failed delivery with that reason.
- **Existing unsigned webhooks.** At start-up elitea-main gives every webhook
  that has no secret a new random one, sealed into the project vault (the
  same start-up pass that moves plain-text secrets into the vault, #2963). The
  log line of that pass counts them as `generated`.

**Who is affected.** A gateway or a provider host deployed without its
identity secret does not start. A webhook receiver that never checked the
signature is not affected. A receiver of a webhook that had no secret now gets
an `X-Webhook-Signature` header on every delivery.

**What to do.**

1. Set `GATEWAY_IDENTITY_SECRET` on the gateway and on elitea-main (the same
   value), and each provider's identity secret on elitea-main and on its host
   (the same value). Use at least 32 random bytes (the #2958 note).
2. For each webhook that had no secret, rotate its secret
   (`POST /webhooks/prompt_lib/{projectID}/{webhookID}/rotate_secret`, or
   **Rotate** on the Webhooks settings page) to get a value you can read, and
   give it to the receiver if it should verify signatures. The generated value
   is never shown.
3. During a rolling upgrade, a replica of the old version can still create a
   webhook without a secret. The new version refuses to deliver for it, with
   the reason in the delivery log, and the next start of elitea-main gives it
   a secret. Rotate its secret to resume deliveries sooner.
