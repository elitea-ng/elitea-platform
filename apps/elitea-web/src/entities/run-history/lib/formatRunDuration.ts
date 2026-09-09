/**
 * A human `Xm Ys` duration string derived from two ISO timestamps.
 *
 * The run-history list's own `duration` field (`ConversationSummary.duration`,
 * from `GET /elitea_core/conversations/prompt_lib/{projectId}`) is a
 * placeholder — `internal/api/v2/conversations/handler.go`'s `List` always
 * answers `-1` for it, no per-conversation duration is computed server-side
 * yet — so the run-history panel derives one client-side from
 * `created_at`/`updated_at` instead of trusting that field.
 */
export function formatRunDuration(createdAt: string | undefined, updatedAt: string | undefined): string {
  if (createdAt === undefined || updatedAt === undefined) return '—';

  const start = new Date(createdAt).getTime();
  const end = new Date(updatedAt).getTime();
  if (Number.isNaN(start) || Number.isNaN(end) || end < start) return '—';

  const totalSeconds = Math.round((end - start) / 1000);
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes === 0) return `${seconds}s`;
  return `${minutes}m ${seconds}s`;
}
