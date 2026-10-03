/**
 * `PipelineWebhookModal.tsx`'s constants and pure helpers, split out for the
 * §3.5 400-line file budget the same way `triggerTypeSelector.lib.ts` is. See
 * that component's own doc comment for the provenance and for the modes
 * (#970, and the GitLab pair from legacy issue 6664).
 */
import type { PipelineInboundTriggerModeRequest } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import type { SingleSelectOption } from '@/shared/ui/SingleSelect';

/**
 * The stored `auth_mode` values a signing trigger carries (`authmode.go`).
 *
 * Module-private: {@link webhookModeFromAuthMode} is the only reader, and an
 * exported constant nothing imports is a public API with no consumer — which
 * is what `check-dead-code` refuses.
 */
const HMAC_AUTH_MODE = 'hmac_sha256';
const STANDARD_WEBHOOKS_AUTH_MODE = 'standard_webhooks_hmac';
const GITLAB_PROVIDER = 'gitlab';

/** What the GitHub preset signs into, and what the dialog shows before the row answers. */
const DEFAULT_SIGNATURE_HEADER = 'X-Hub-Signature-256';

/** The Standard Webhooks signature header — fixed by that specification, and what a GitLab signing token sends. */
const STANDARD_WEBHOOKS_SIGNATURE_HEADER = 'webhook-signature';

/** The header a GitLab webhook sends its secret token in — a bearer carrier the backend reads. */
const GITLAB_TOKEN_HEADER = 'X-Gitlab-Token';

/** The four modes this dialog offers. See `PipelineWebhookModal.tsx`'s header for what each one verifies. */
export const WEBHOOK_MODES = {
  custom: 'custom',
  github: 'github',
  gitlab: 'gitlab',
  gitlabSigning: 'gitlab_signing',
} as const;

export type WebhookMode = (typeof WEBHOOK_MODES)[keyof typeof WEBHOOK_MODES];

export const MODE_OPTIONS: SingleSelectOption[] = [
  { label: 'Custom (bearer secret)', value: WEBHOOK_MODES.custom },
  { label: 'GitHub (signed payload)', value: WEBHOOK_MODES.github },
  { label: 'GitLab (secret token)', value: WEBHOOK_MODES.gitlab },
  { label: 'GitLab (signing token)', value: WEBHOOK_MODES.gitlabSigning },
];

/** A select value back to a mode. Anything unknown is Custom, which is what a stored row with no mode is too. */
export function webhookModeFromValue(value: string): WebhookMode {
  const known: readonly string[] = Object.values(WEBHOOK_MODES);
  return known.includes(value) ? (value as WebhookMode) : WEBHOOK_MODES.custom;
}

/**
 * The create/rotate body for a mode. The backend writes the mode on that route
 * and nowhere else, so this is the whole of "apply this mode".
 */
export function modeRequest(mode: WebhookMode): PipelineInboundTriggerModeRequest {
  switch (mode) {
    case WEBHOOK_MODES.github:
      return { type: 'github' };
    case WEBHOOK_MODES.gitlab:
      return { type: 'gitlab' };
    case WEBHOOK_MODES.gitlabSigning:
      return { type: 'gitlab', auth_mode: 'standard_webhooks_hmac' };
    case WEBHOOK_MODES.custom:
      return { type: 'custom' };
  }
}

/**
 * The signature header a mode reads, or `undefined` for a bearer mode. The
 * stored value wins for GitHub, because the explicit form lets a row sign into
 * a header of its own.
 */
export function signatureHeaderFor(mode: WebhookMode, stored: string | undefined): string | undefined {
  if (mode === WEBHOOK_MODES.github) return stored ?? DEFAULT_SIGNATURE_HEADER;
  if (mode === WEBHOOK_MODES.gitlabSigning) return STANDARD_WEBHOOKS_SIGNATURE_HEADER;
  return undefined;
}

/**
 * The header form is what the backend's own documentation shows first: a URL
 * is written to proxy logs, and a credential in one outlives the request.
 *
 * A SIGNING trigger gets a different example, because the bearer form does not
 * work on one: it is refused, deliberately, so that a leaked URL plus the
 * secret a sender pasted into a provider form cannot start runs. What is shown
 * instead is how the signature is computed, since the sender computes it.
 */
export function buildExampleRequest(args: { url: string; secret: string | undefined; showSecret: boolean; mode: WebhookMode; signatureHeader: string | undefined }): string | null {
  const { url, secret, showSecret, mode, signatureHeader } = args;
  if (!url) return null;
  const displaySecret = showSecret && secret !== undefined ? secret : '<your_secret>';
  if (mode === WEBHOOK_MODES.gitlabSigning) {
    return [
      `BODY='{"object_kind": "push"}'`,
      `ID="msg_$(date +%s)"; TS=$(date +%s)`,
      `KEY=$(printf '%s' "${displaySecret}" | sed 's/^whsec_//' | base64 -d | xxd -p -c 256)`,
      `SIGNATURE=$(printf '%s' "$ID.$TS.$BODY" | openssl dgst -sha256 -mac HMAC -macopt hexkey:$KEY -binary | base64)`,
      `curl -X POST "${url}" \\`,
      `  -H "Content-Type: application/json" \\`,
      `  -H "webhook-id: $ID" -H "webhook-timestamp: $TS" \\`,
      `  -H "${STANDARD_WEBHOOKS_SIGNATURE_HEADER}: v1,$SIGNATURE" \\`,
      `  -d "$BODY"`,
    ].join('\n');
  }
  if (signatureHeader !== undefined && signatureHeader !== '') {
    return [
      `BODY='{"ref": "refs/heads/main"}'`,
      `SIGNATURE=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "${displaySecret}" | awk '{print $2}')`,
      `curl -X POST "${url}" \\`,
      `  -H "Content-Type: application/json" \\`,
      `  -H "${signatureHeader}: sha256=$SIGNATURE" \\`,
      `  -d "$BODY"`,
    ].join('\n');
  }
  if (mode === WEBHOOK_MODES.gitlab) {
    return `curl -X POST "${url}" \\\n  -H "Content-Type: application/json" \\\n  -H "${GITLAB_TOKEN_HEADER}: ${displaySecret}" \\\n  -d '{"object_kind": "push"}'`;
  }
  return `curl -X POST "${url}" \\\n  -H "Content-Type: application/json" \\\n  -H "Authorization: Bearer ${displaySecret}" \\\n  -d '{"input": "Your message or data here"}'`;
}

/**
 * The stored `auth_mode` and `provider` as the dialog's own choice. A bearer
 * row is GitLab when its provider says so, and Custom otherwise — which is
 * what an older row with no mode at all is.
 */
export function webhookModeFromAuthMode(authMode: string | undefined, provider?: string): WebhookMode {
  if (authMode === HMAC_AUTH_MODE) return WEBHOOK_MODES.github;
  if (authMode === STANDARD_WEBHOOKS_AUTH_MODE) return WEBHOOK_MODES.gitlabSigning;
  return provider === GITLAB_PROVIDER ? WEBHOOK_MODES.gitlab : WEBHOOK_MODES.custom;
}

/** The one-sentence explanation of what a mode verifies. */
export function modeHint(mode: WebhookMode, signatureHeader: string | undefined): string {
  switch (mode) {
    case WEBHOOK_MODES.github:
      return t(
        'pipelines.pipelineWebhookModal.modeGithubHint',
        'GitHub signs the request body with the secret and sends the digest in {{header}}. Configure that secret in the repository\u2019s webhook settings; no Authorization header is sent or accepted.',
        { header: signatureHeader ?? DEFAULT_SIGNATURE_HEADER },
      );
    case WEBHOOK_MODES.gitlab:
      return t(
        'pipelines.pipelineWebhookModal.modeGitlabHint',
        'GitLab sends the secret token verbatim in {{header}}. Paste the secret into the webhook\u2019s Secret token field in GitLab.',
        { header: GITLAB_TOKEN_HEADER },
      );
    case WEBHOOK_MODES.gitlabSigning:
      return t(
        'pipelines.pipelineWebhookModal.modeGitlabSigningHint',
        'GitLab signs webhook-id, webhook-timestamp and the request body with the signing token and sends it in webhook-signature. Deliveries older than five minutes are refused.',
      );
    case WEBHOOK_MODES.custom:
      return t(
        'pipelines.pipelineWebhookModal.modeCustomHint',
        'The sender presents the secret itself, as an Authorization: Bearer header, as X-Elitea-Trigger-Token, or as a token query parameter.',
      );
  }
}
