/**
 * "Remember this" on an assistant message (#870):
 * saves the message's own text as a persistent, cross-conversation personal
 * memory (`internal/api/v2/memories`), stamping `source_conversation_id` so
 * Settings > Memory's list can show where it came from.
 *
 * A SELF-CONTAINED control — same shape `MessageFeedbackControl.tsx` (#880)
 * already established for a per-message action: it owns its own write
 * rather than threading a callback through `ApplicationAnswer`'s already-
 * large prop surface, and it degrades to rendering nothing whenever it has
 * no project id to call with (same gate `showFeedback` uses in
 * `ApplicationAnswer.tsx`).
 *
 * Two halves: `useRememberMemory`, held by the reply's action row, and
 * `RememberMemoryMenuRow`, the row in that row's ⋯ menu. The menu closes on
 * the click, so the save's state (pending, "Saved", the failure toast) lives
 * in the hook, outside it, and outlasts the menu.
 */
import type { ReactNode } from 'react';
import { useCallback, useState } from 'react';

import { useMutation } from '@tanstack/react-query';

import BookmarkAddOutlinedIcon from '@mui/icons-material/BookmarkAddOutlined';
import BookmarkAddedIcon from '@mui/icons-material/BookmarkAdded';
import Snackbar from '@mui/material/Snackbar';
import Alert from '@mui/material/Alert';

import { t } from '@/shared/i18n';
import type { MemoryEntryWriteRequest } from '@/shared/api/generated/model';
import { createMemory } from '@/shared/api/generated/chat/chat';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { MenuRow, MenuRowIcon, MenuRowText } from '@/shared/ui/native-menu';

/**
 * The SERVER's own explanation, when it sent one. Duplicated from
 * `features/settings/lib/memory/longTermMemoryHelpers.ts`'s
 * `memoryServerErrorMessage` rather than imported: `no-sideways-features`
 * exempts `features/chat-messages` as an importer (dependency-cruiser.cjs's
 * own `pathNot`), but this codebase's established answer even where the
 * gate would allow it is a dozen duplicated lines over a cross-feature
 * reach — see `webhookHelpers.ts`'s identical helper and its own comment.
 */
function memoryServerErrorMessage(error: unknown, fallback: string): string {
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

export interface RememberMemoryParams {
  readonly projectId: string;
  readonly content: string;
  /** The owning conversation's uuid, stamped as `source_conversation_id` when known. */
  readonly conversationId?: string | undefined;
}

export interface RememberMemory {
  /** False with no project id or no text: the menu offers no row. */
  readonly available: boolean;
  readonly save: () => void;
  readonly isPending: boolean;
  readonly justSaved: boolean;
  /** The failure toast; the caller renders it outside the menu. */
  readonly toast: ReactNode;
}

/** How long the "Saved" confirmation state shows before the row reverts — long enough to notice, short enough that a second save is never blocked by it. */
const SAVED_STATE_MS = 2500;

export function useRememberMemory({ projectId, content, conversationId }: RememberMemoryParams): RememberMemory {
  const [justSaved, setJustSaved] = useState(false);
  const [errorMessage, setErrorMessage] = useState<string | null>(null);

  const saveMutation = useMutation({
    mutationFn: (body: MemoryEntryWriteRequest) => createMemory(projectId, body),
  });

  const save = useCallback(() => {
    const trimmed = content.trim();
    if (!trimmed) return;
    saveMutation.mutate(
      {
        content: trimmed,
        ...(conversationId ? { source_conversation_id: conversationId } : {}),
        enabled: true,
      },
      {
        onSuccess: () => {
          setJustSaved(true);
          setTimeout(() => setJustSaved(false), SAVED_STATE_MS);
        },
        onError: (error) => {
          setErrorMessage(memoryServerErrorMessage(error, t('chat.rememberThis.error', 'Failed to save this as a memory')));
        },
      },
    );
  }, [content, conversationId, saveMutation]);

  const toast = (
    <Snackbar
      open={errorMessage !== null}
      autoHideDuration={4000}
      onClose={() => setErrorMessage(null)}
      anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}
    >
      {errorMessage ? (
        <Alert onClose={() => setErrorMessage(null)} severity="error" variant="filled">
          {errorMessage}
        </Alert>
      ) : undefined}
    </Snackbar>
  );

  return { available: Boolean(projectId) && content.trim() !== '', save, isPending: saveMutation.isPending, justSaved, toast };
}

export function RememberMemoryMenuRow({ memory, disabled = false, onDone }: { readonly memory: RememberMemory; readonly disabled?: boolean; readonly onDone: () => void }): ReactNode {
  if (!memory.available) return null;
  return (
    <MenuRow
      data-testid="remember-memory-action"
      disabled={disabled || memory.isPending}
      onClick={() => {
        memory.save();
        onDone();
      }}
    >
      <MenuRowIcon>
        {memory.justSaved ? <BookmarkAddedIcon data-testid="remember-memory-saved-icon" /> : <BookmarkAddOutlinedIcon />}
      </MenuRowIcon>
      <MenuRowText>{memory.justSaved ? t('chat.rememberThis.saved', 'Saved to memory') : t('chat.rememberThis.tooltip', 'Remember this')}</MenuRowText>
    </MenuRow>
  );
}
