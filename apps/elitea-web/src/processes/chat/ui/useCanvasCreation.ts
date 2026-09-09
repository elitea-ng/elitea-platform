/**
 * MAKING a canvas — the half of the canvas feature that had no caller.
 *
 * `useCanvasEditing` next door opens, edits and saves an EXISTING canvas. The
 * create route had none of that: `useCreateCanvasMutation` was exported from
 * `entities/canvas` and called by nothing, its params did not even carry the
 * range the route splits on, and the streaming journey's own header said the
 * gesture "has no control in this app yet" and drove the route through
 * `page.request` instead. This hook is that control's other end.
 *
 * ── WHY THE TRANSCRIPT CANNOT NAME THE ROW ON ITS OWN ─────────────────────
 * The create takes THREE things the rendered transcript does not hold:
 *
 *  1. `message_group_id` — the group's ROW id. `entities/message` normalises a
 *     group's `id` to its UUID, so the transcript knows the uuid and not the
 *     number.
 *  2. `message_item_id` — the `text_message` row that gets split. The
 *     paginated messages route the chat page reads COLLAPSES a group's text
 *     into `content` and serves no text item at all, so on that route the id
 *     does not reach the client in any form.
 *  3. The STORED text, byte for byte. The reader selected rendered markdown;
 *     the offsets are measured against the source.
 *
 * All three come from one read — the conversation-details route, which serves
 * `message_groups` with their items — and it is made HERE, at create time,
 * rather than kept in a cache. Two reasons, and the second is the one that
 * matters: a cached copy would be a second source of truth for offsets that
 * the create itself invalidates (it deletes the item it splits and writes new
 * ones), so the very next selection in the same answer would be measured
 * against a row that no longer exists.
 *
 * ── AFTER THE CREATE ──────────────────────────────────────────────────────
 * The transcript is re-read, not patched. The split rewrites one item into
 * three, and a client that spliced its own guess of that result into the
 * message list would be re-deriving the server's split rule — the shape that
 * produced this app's earlier canvas defects. Invalidating the two
 * conversation queries makes the page fetch what the server actually wrote,
 * and `useChatBoxData`'s re-seed adopts a NON-EMPTY server answer for the same
 * conversation, so the block appears without a reload.
 */
import { useCallback } from 'react';

import { useQueryClient } from '@tanstack/react-query';
import { useParams } from '@tanstack/react-router';

import { conversationApi } from '@/entities/conversation';
import { useCreateCanvasMutation } from '@/entities/canvas';
import type { AnswerCanvasSelection } from '@/features/chat-messages';
import { canvasByteRange, canvasKindForSelection } from '@/features/chat-messages';
import { useSelectedProject } from '@/widgets/app-shell';

/** One stored item, read defensively off the details payload. */
interface StoredItem {
  readonly id?: string | number;
  readonly item_type?: string;
  readonly item_details?: unknown;
}

/** One stored group, read defensively off the details payload. */
interface StoredGroup {
  readonly id?: string | number;
  readonly uuid?: string;
  readonly message_items?: readonly StoredItem[];
}

/** The text an item holds, or `''` — `item_details.content` is the only place a `text_message` states it. */
function itemText(item: StoredItem): string {
  const details = item.item_details;
  if (typeof details !== 'object' || details === null) return '';
  const content = (details as { content?: unknown }).content;
  return typeof content === 'string' ? content : '';
}

/**
 * The stored text item the selection came out of.
 *
 * The row id the transcript supplied is a PREFERENCE, not the answer: it is
 * absent on the route the page reads, and it can name an item that no longer
 * holds these words after an earlier create split it. So the item is chosen by
 * the words it actually contains, and the id only breaks a tie between two
 * items that both contain them.
 */
export function findSelectedItem(group: StoredGroup, selectedText: string, preferredId: number | undefined): StoredItem | undefined {
  const needle = selectedText.trim();
  const candidates = (group.message_items ?? []).filter(
    (item) => item.item_type === 'text_message' && needle !== '' && itemText(item).includes(needle),
  );
  return candidates.find((item) => Number(item.id) === preferredId) ?? candidates[0];
}

/** The group the transcript named, matched on either spelling of its identity. */
export function findGroup(groups: readonly StoredGroup[], messageGroupUuid: string): StoredGroup | undefined {
  return groups.find((group) => String(group.uuid ?? '') === messageGroupUuid || String(group.id ?? '') === messageGroupUuid);
}

export interface UseCanvasCreationResult {
  /** Carves the highlighted range of an answer into a canvas, then re-reads the transcript. */
  readonly onCreateCanvasFromSelection: (selection: AnswerCanvasSelection) => void;
}

export function useCanvasCreation(): UseCanvasCreationResult {
  const { project } = useSelectedProject();
  const projectId = project?.id === undefined ? undefined : String(project.id);
  const params = useParams({ strict: false }) as { conversationId?: string };
  const conversationId = params.conversationId;
  const queryClient = useQueryClient();
  const { mutateAsync: createCanvas } = useCreateCanvasMutation();

  const create = useCallback(
    async (selection: AnswerCanvasSelection): Promise<void> => {
      if (projectId === undefined || conversationId === undefined || conversationId === '') return;

      const details = await conversationApi.details({
        projectId,
        id: conversationId,
        messages_limit: 100,
        sort_order: 'asc',
      });
      const groups = (details['message_groups'] ?? []) as readonly StoredGroup[];
      const group = findGroup(groups, selection.messageGroupUuid);
      if (group === undefined) return;

      const item = findSelectedItem(group, selection.selectedText, selection.messageItemId);
      if (item === undefined) return;
      const range = canvasByteRange(itemText(item), selection.selectedText);
      if (range === undefined) return;

      // Issue #879: a carved-out range that reads as PROSE becomes a
      // `document` canvas (rich-text, Markdown round-trip) rather than a
      // `code` one — `selection.kind` overrides the guess for a caller that
      // already knows (the "Open as document" answer action always forces
      // it); the selection-drag affordance leaves it unset and gets
      // `canvasKindForSelection`'s guess from the text shape.
      const kind = selection.kind ?? canvasKindForSelection(selection.selectedText);
      const isDocument = kind === 'document';

      await createCanvas({
        projectId,
        message_group_id: Number(group.id),
        message_item_id: Number(item.id),
        // The name the block's own title renders, and the one the editor's
        // header derives from the language — the reference's own string for a
        // canvas whose document is prose or code rather than a table or a
        // diagram.
        name: isDocument ? 'Edit document' : 'Edit code',
        canvas_type: isDocument ? 'document' : 'code',
        code_language: isDocument ? 'document' : 'markdown',
        canvas_content_starts_at: range.startsAt,
        canvas_content_ends_at: range.endsAt,
      });

      await queryClient.invalidateQueries({ queryKey: ['conversation', 'messageList'] });
      await queryClient.invalidateQueries({ queryKey: ['conversation', 'details'] });
    },
    [conversationId, createCanvas, projectId, queryClient],
  );

  const onCreateCanvasFromSelection = useCallback(
    (selection: AnswerCanvasSelection) => {
      // Fire-and-forget, and the failure is swallowed HERE only because this
      // app has no toast hook at this layer yet — the same statement
      // `useCanvasEditing`'s own save carries. A refused create leaves the
      // answer exactly as it was, which is the server's own guarantee: it
      // refuses an out-of-range selection rather than clamping it.
      void create(selection).catch(() => undefined);
    },
    [create],
  );

  return { onCreateCanvasFromSelection };
}
