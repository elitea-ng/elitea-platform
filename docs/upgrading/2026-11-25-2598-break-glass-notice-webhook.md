# Break-glass: an optional out-of-band notice webhook (#2598, ADR-0059)

**What changed.** The break-glass notices can go to a webhook as well as by
e-mail, for when the mail server is down at the moment break-glass is used.
The new optional chart key `main.breakGlass.noticeWebhook` takes a
reference to an existing Secret, never a URL: a Slack, Teams or PagerDuty
webhook URL carries its token, so it is a credential.

- `existingSecret`: the Secret's name. Empty (the default) sends no webhook.
- `key` (default `url`): the Secret's key holding the `https` URL.
- `signingSecretKey` (default `signing-secret`): the Secret's key holding the
  signing secret, at least 32 random bytes.
- `egressAllowlist`: private destinations the webhook may reach; empty
  permits public destinations only.

The chart's schema refuses a `url` key under `main.breakGlass.noticeWebhook`,
and the template refuses an `egressAllowlist` without `existingSecret`.
Main reads the three `ELITEA_BREAK_GLASS_NOTICE_WEBHOOK_*` variables at
start and refuses to start on a URL that is not `https` or a weak signing
secret.

An activation, a critical change under an elevation, an overdue review, an
elevation left with no reviewer and an `unauthorized` review each make one
signed `POST` (`X-Webhook-Signature: sha256=<HMAC of the body>`) with ids, a
type and a time only. It names no person. A failed call waits in the same
outbox as the e-mails and is retried by the `main.access_notice_sender`
routine on its own, without holding the e-mails.

The shared migration `0281_admin_access_notice_webhook.sql` adds the
`webhook` channel to `elitea_auth.admin_access_notices`: a webhook row has no
recipient person. It changes no existing row.

**Who is affected.** Only a deployment that sets the key. Without it,
nothing changes.

**What to do.** Optional. To use it, create the Secret and set
`existingSecret`; see "The out-of-band notice webhook" in
[Configuration](../../apps/elitea-docs/src/content/docs/deployment/configuration.mdx).
