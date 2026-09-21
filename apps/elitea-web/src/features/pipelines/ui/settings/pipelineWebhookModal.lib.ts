/**
 * `PipelineWebhookModal.tsx`'s constants and pure helpers, split out for the
 * §3.5 400-line file budget the same way `triggerTypeSelector.lib.ts` is. See
 * that component's own doc comment for the provenance and for #970's two
 * modes.
 */
import type { SingleSelectOption } from '@/shared/ui/SingleSelect';

/** The stored `auth_mode` a signing trigger carries (`authmode.go`). */
export const HMAC_AUTH_MODE = 'hmac_sha256';

/** What the GitHub preset signs into, and what the dialog shows before the row answers. */
export const DEFAULT_SIGNATURE_HEADER = 'X-Hub-Signature-256';

/** The two modes this dialog offers. See the file header for why not three. */
export const WEBHOOK_MODES = {
  custom: 'custom',
  github: 'github',
} as const;

export type WebhookMode = (typeof WEBHOOK_MODES)[keyof typeof WEBHOOK_MODES];

export const MODE_OPTIONS: SingleSelectOption[] = [
  { label: 'Custom (bearer secret)', value: WEBHOOK_MODES.custom },
  { label: 'GitHub (signed payload)', value: WEBHOOK_MODES.github },
];

/**
 * The header form is what the backend's own documentation shows first: a URL
 * is written to proxy logs, and a credential in one outlives the request.
 *
 * A SIGNING trigger gets a different example, because the bearer form does not
 * work on one: it is refused, deliberately, so that a leaked URL plus the
 * secret a sender pasted into a provider form cannot start runs. What is shown
 * instead is how the signature is computed, since the sender computes it.
 */
export function buildExampleRequest(args: { url: string; secret: string | undefined; showSecret: boolean; signatureHeader: string | undefined }): string | null {
  const { url, secret, showSecret, signatureHeader } = args;
  if (!url) return null;
  const displaySecret = showSecret && secret !== undefined ? secret : '<your_secret>';
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
  return `curl -X POST "${url}" \\\n  -H "Content-Type: application/json" \\\n  -H "Authorization: Bearer ${displaySecret}" \\\n  -d '{"input": "Your message or data here"}'`;
}


/** The stored `auth_mode` as the dialog's own choice. Anything but the signing mode is Custom, which is what an older row with no mode at all is. */
export function webhookModeFromAuthMode(authMode: string | undefined): WebhookMode {
  return authMode === HMAC_AUTH_MODE ? WEBHOOK_MODES.github : WEBHOOK_MODES.custom;
}
