/**
 * The `/chat` page's add-participant flow (gap G2).
 *
 * `AddNewUserModal` — the picker, its search over the project's user listing
 * and its "Add Selected" action — was ported whole and had ZERO call sites
 * anywhere in the app. `ParticipantsWrapper` had always computed a
 * `disabledAdd` flag (playback state plus the caller's
 * `configuration.users.users.view` grant) and threaded it down to a control
 * that did not exist, so the flag decided the state of nothing: a conversation
 * could gain an agent from the composer's "+" menu and could not gain a
 * colleague by any route at all.
 *
 * Its own module rather than a block inside `ChatPage` because that component
 * sits on its §3.5 cyclomatic-complexity ceiling, and because "can this
 * conversation take a participant at all" is a rule worth stating once: `open`
 * is `undefined` while the chat has no persisted conversation, which is what
 * makes the panel withhold the control instead of offering one that cannot
 * work. A participant is a row against a conversation id, and a draft chat has
 * none until the first send creates one.
 */
import { useCallback, useState } from 'react';

import { useAddParticipantMutation } from '@/entities/participant';

/** @public What `ChatPage` needs to render the panel control and the picker. */
export interface UseAddParticipantsResult {
  /** Whether the picker is open. */
  readonly isOpen: boolean;
  /** Opens the picker — `undefined` when this conversation cannot take a participant yet. */
  readonly open: (() => void) | undefined;
  /** Closes the picker without adding anybody. */
  readonly close: () => void;
  /** Attaches the people the picker returned. */
  readonly addUsers: (users: readonly Record<string, unknown>[]) => void;
}

/** One participant row, in the shape `AddParticipant`'s handler reads. */
function toUserParticipant(id: string | number): { entity_name: string; entity_meta: Record<string, unknown>; entity_settings: Record<string, unknown> } {
  // The id ALONE. `ListParticipants` resolves the display name server-side
  // from `auth_core__user` into `meta.user_name`; a name sent from here would
  // put this browser's idea of the person onto everyone else's screen.
  return { entity_name: 'user', entity_meta: { id: String(id) }, entity_settings: {} };
}

function isId(value: unknown): value is string | number {
  return typeof value === 'string' || typeof value === 'number';
}

export function useAddParticipants(params: {
  readonly projectId: string | number | undefined;
  readonly conversationId: string | undefined;
}): UseAddParticipantsResult {
  const { projectId, conversationId } = params;
  const [isOpen, setIsOpen] = useState(false);
  const { mutate: addParticipants } = useAddParticipantMutation();

  const close = useCallback(() => setIsOpen(false), []);
  const openPicker = useCallback(() => setIsOpen(true), []);

  /**
   * `useAddParticipantMutation` invalidates the conversation-details query, so
   * the rail re-reads the participant list the SERVER holds rather than a list
   * this callback patched locally — which is what makes the rendered row
   * evidence of the attach rather than of the click.
   */
  const addUsers = useCallback(
    (users: readonly Record<string, unknown>[]) => {
      if (projectId === undefined || conversationId === undefined) return;
      const rows = users.map((user) => user['id']).filter(isId).map(toUserParticipant);
      if (rows.length === 0) return;
      addParticipants({ projectId, conversationId, participants: rows });
    },
    [addParticipants, conversationId, projectId],
  );

  return {
    isOpen,
    open: conversationId === undefined ? undefined : openPicker,
    close,
    addUsers,
  };
}
