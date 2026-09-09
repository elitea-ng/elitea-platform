/**
 * Pure helpers for the project Webhooks surface (#876).
 *
 * Kept separate from the components so the secret-generation shape can be
 * unit-tested without mounting React.
 *
 * `parseWebhookEvents`/`formatWebhookEvents` (the comma-separated-string
 * helpers for the free-text events field) were removed when #876's second
 * half swapped that field for `WebhookFormDialog`'s Autocomplete picker
 * (`webhookEventCatalogue.ts`), which works on `string[]` directly and needs
 * no comma-joining. Leaving unreferenced exports behind them would be the
 * exact dead-code-with-no-caller class this codebase's own knip/E2E gates
 * exist to catch.
 */

/**
 * A random URL-safe secret, matching the shape
 * `PipelineWebhookModal.tsx`'s `generateSecretToken` already uses for the
 * pipeline INBOUND trigger — the same 256 bits of `crypto.getRandomValues`,
 * base64url-encoded. There is no server-side secret generation for this
 * resource (`internal/api/webhook/handler.go`'s Create stores whatever the
 * caller sends), so the client is the only place a fresh value comes from.
 */
export function generateWebhookSecret(): string {
  const array = new Uint8Array(32);
  crypto.getRandomValues(array);
  return btoa(String.fromCharCode(...array)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=/g, '');
}

/** A short masked placeholder for a hidden secret — length-independent, unlike `'•'.repeat(n)`. */
export const MASKED_SECRET = '••••••••••••••••';
