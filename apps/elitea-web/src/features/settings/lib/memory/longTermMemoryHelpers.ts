/**
 * Pure helpers for the Long-term Memory surface (#870): Settings > Memory's
 * "Long-term Memory" panel (`ui/memory/LongTermMemoryManagement.tsx`) and
 * the chat message "Remember this" action
 * (`features/chat-messages/ui/chat-box/RememberMemoryAction.tsx`).
 *
 * Kept separate from the components so they can be unit-tested without
 * mounting React. A refusal's text comes from the shared
 * `serverErrorMessage` (`shared/api/serverErrorMessage.ts`).
 */

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
