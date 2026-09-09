/**
 * Pure helpers for the Long-term Memory surface (#870): Settings > Memory's
 * "Long-term Memory" panel (`ui/memory/LongTermMemoryManagement.tsx`) and
 * the chat message "Remember this" action
 * (`features/chat-messages/ui/chat-box/RememberMemoryAction.tsx`).
 *
 * Kept separate from the components so the server-error extraction can be
 * unit-tested without mounting React — same split
 * `features/settings/lib/webhooks/webhookHelpers.ts` makes for the same
 * reason.
 */
import { EliteaApiError } from '@/shared/api/generated/mutator';

/**
 * The SERVER's own explanation, when it sent one
 * (internal/api/v2/memories/handler.go answers `{"error": "..."}` via
 * apierr.Write on a 400/404). Same extraction
 * `features/settings/lib/webhooks/webhookHelpers.ts`'s
 * `webhookServerErrorMessage` already does — rebuilt here rather than
 * imported, since `no-sideways-features` forbids reaching into another
 * feature's internals.
 */
export function memoryServerErrorMessage(error: unknown, fallback: string): string {
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
 * Splits a comma-separated tags field into trimmed, non-empty entries —
 * the form's free-text tags input (`LongTermMemoryFormDialog.tsx`) is a
 * single text field rather than webhooks' Autocomplete picker, since
 * memory tags are the user's own free-form labels with no catalogue to
 * pick from (unlike webhook event types).
 */
export function parseMemoryTags(raw: string): string[] {
  return raw
    .split(',')
    .map((tag) => tag.trim())
    .filter((tag) => tag !== '');
}

/** The inverse of `parseMemoryTags`, for re-populating the form's edit state. */
export function formatMemoryTags(tags: readonly string[]): string {
  return tags.join(', ');
}
