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
import { EliteaApiError } from '@/shared/api/generated/mutator';

/**
 * The SERVER's own explanation, when it sent one — same extraction
 * `features/settings/api/ai-configuration/api.ts`'s
 * `modelConfigurationErrorMessage` and `features/agents/lib/errorMessage.ts`'s
 * `applicationServerErrorMessage` already do (see either's own doc comment
 * for the full rationale). Rebuilt here rather than imported:
 * `no-sideways-features` forbids reaching into another feature's internals;
 * a dozen lines duplicated is this codebase's established answer to that.
 *
 * The SSRF hardening (issue 876 follow-up) is what makes this matter for
 * webhooks specifically: `internal/api/webhook/handler.go`'s Create and
 * Update now answer 400 with a real, specific reason ("... resolves to a
 * private-network address ...") whenever a destination is refused, and
 * `EliteaApiError.message` alone is only the `eliteaFetch: 400 from <url>`
 * diagnostic — putting THAT in front of a user who just typed a bad URL
 * would tell them nothing they did not already know from the form turning
 * red.
 */
export function webhookServerErrorMessage(error: unknown, fallback: string): string {
  if (error instanceof EliteaApiError && error.failure.kind === 'http') {
    const { body } = error.failure;
    if (typeof body === 'string' && body !== '') return body;
    if (typeof body === 'object' && body !== null) {
      const record = body as Record<string, unknown>;
      const detail = record['error'] ?? record['message'];
      if (typeof detail === 'string' && detail !== '') return detail;
    }
  }
  return fallback;
}

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
