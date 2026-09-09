/**
 * Pure helpers for the project Webhooks surface (#876).
 *
 * Kept separate from the components so the secret-generation shape and the
 * comma-separated events field can be unit-tested without mounting React.
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

/** Splits the form's comma-separated events field into a trimmed, non-empty list. */
export function parseWebhookEvents(raw: string): string[] {
  return raw
    .split(',')
    .map((event) => event.trim())
    .filter((event) => event.length > 0);
}

/** The reverse of {@link parseWebhookEvents}, for populating an edit form. */
export function formatWebhookEvents(events: readonly string[]): string {
  return events.join(', ');
}

/** A short masked placeholder for a hidden secret — length-independent, unlike `'•'.repeat(n)`. */
export const MASKED_SECRET = '••••••••••••••••';
