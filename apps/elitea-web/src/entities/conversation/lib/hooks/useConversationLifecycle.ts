import { useCallback, useRef, useState } from 'react';

import {
  conversationCreate,
  conversationDetails,
  conversationEdit,
  deleteConversation as deleteConversationRequest,
  selectConversation as selectConversationRequest,
  unselectConversation as unselectConversationRequest,
  type ConversationWire,
} from '../../api/conversationApi';

/**
 * Ported from `apps/elitea-ui/src/hooks/chat/{useCreateConversation,
 * useSelectConversation,useDeleteConversation}.js` + `[fsd]/features/chat/
 * lib/hooks/useEditConversation.hooks.js` (unit C1) — as ONE bundled hook of
 * thin imperative actions calling `../../api/conversationApi.ts`, each with
 * its own loading/error state.
 *
 * **DISCLOSED SCOPE CUT, same class of deviation as `entities/
 * application-form/model/mutations.ts`'s own doc comment.** The four
 * baseline hooks are soaked in page/feature orchestration that has no
 * legal home in `entities/` (`no-upward-from-entities`): socket room
 * enter/leave (`emitEnterRoom`/`emitLeaveRoom`), canvas-editor socket
 * listener wiring, toast surfacing, GA analytics, URL navigation
 * (`changeUrlByConversation`), local-active-participant restoration, and
 * `folders`-array bookkeeping (a SEPARATE `entities/folder` concern, not
 * named in this unit's brief). None of that is "conversation entity" domain
 * logic — it's page composition. What IS ported, faithfully: the REST calls
 * themselves, in the same combinations the baseline makes them (delete
 * skips the network call for a playback row; select fetches details +
 * marks-selected together). A caller that needs the baseline's own
 * "is this the currently-active conversation" gate should compare
 * identities with `shared/lib/chat.ts`'s real `areTheSameConversations` —
 * legally importable from any layer, not re-exported here.
 *
 * **Also verified out of scope (adversarial-verify follow-up):**
 * `useSelectConversation.js`'s `inFlightConversationIdRef`/
 * `prevSelectedConversationIdRef` (its only such guards —
 * `useDeleteConversation.js` has none) are both same-target no-op checks
 * keyed by `(id, isPlayback)`, not a general concurrency mutex — they
 * exist to paper over `activeConversation` being an async prop that lags
 * behind a click (skip re-selecting what's already the target; skip a
 * second click on the same id while its own fetch is in flight). Neither
 * blocks a DIFFERENT id's call while another is in flight. Same class of
 * cut as `changeUrlByConversation`/toast/GA above: real page-state
 * (`activeConversation`) this hook deliberately doesn't hold.
 *
 * `projectId` is an explicit parameter (N4 signature deviation, same
 * reasoning as `useChatStreaming.ts`).
 */

export interface NewConversationInput {
  readonly name: string;
  readonly isPrivate: boolean;
  readonly participants?: readonly unknown[];
  readonly meta?: Readonly<Record<string, unknown>>;
}

export interface EditConversationInput {
  readonly id: string | number;
  readonly name?: string;
  readonly isPrivate?: boolean;
}

export interface ConversationIdentity {
  readonly id: string | number;
  readonly isPlayback?: boolean;
}

export interface UseConversationLifecycleResult {
  readonly createConversation: (input: NewConversationInput) => Promise<ConversationWire | undefined>;
  readonly isCreating: boolean;
  readonly createError: unknown;

  readonly editConversation: (input: EditConversationInput) => Promise<ConversationWire | undefined>;
  readonly isEditing: boolean;
  readonly editError: unknown;

  /** `false` (deletion refused) only when the REST call itself fails; a playback row always "succeeds" without a network call (baseline: `useDeleteConversation.js:63-68`). */
  readonly deleteConversation: (conversation: ConversationIdentity) => Promise<boolean>;
  readonly isDeleting: boolean;
  readonly deleteError: unknown;

  /** `undefined` for a playback row (baseline skips the network fetch entirely, `useSelectConversation.js:130-137`) — the caller already has the full playback conversation object locally. */
  readonly selectConversation: (conversation: ConversationIdentity) => Promise<ConversationWire | undefined>;
  readonly isSelecting: boolean;
  readonly selectError: unknown;

  readonly unselectConversation: () => Promise<void>;
}

function useAsyncAction<A extends unknown[], R>(action: (...args: A) => Promise<R>): { readonly run: (...args: A) => Promise<R | undefined>; readonly isRunning: boolean; readonly error: unknown } {
  const [isRunning, setIsRunning] = useState(false);
  const [error, setError] = useState<unknown>(undefined);

  // `action` is a fresh closure every render (each of the 5 call sites
  // below is an inline arrow in useConversationLifecycle's body) — a
  // ref keeps `run`'s own identity stable across renders (deps: []) while
  // still always invoking the latest closure (so a `projectId` change,
  // e.g. project switch, is never served by a stale one either). Found by
  // adversarial verify: the previous `[action]` dep made `run` referentially
  // unstable on every render, which would defeat memoization for any
  // consumer that puts e.g. `deleteConversation` in its own deps array.
  const actionRef = useRef(action);
  actionRef.current = action;

  const run = useCallback(async (...args: A): Promise<R | undefined> => {
    setIsRunning(true);
    setError(undefined);
    try {
      return await actionRef.current(...args);
    } catch (caught) {
      setError(caught);
      return undefined;
    } finally {
      setIsRunning(false);
    }
  }, []);

  return { run, isRunning, error };
}

export function useConversationLifecycle(projectId: string | number | undefined): UseConversationLifecycleResult {
  const create = useAsyncAction(async (input: NewConversationInput): Promise<ConversationWire | undefined> => {
    if (projectId === undefined) return undefined;
    return conversationCreate({ projectId, name: input.name, is_private: input.isPrivate, participants: input.participants ?? [], meta: input.meta ?? {} });
  });

  const edit = useAsyncAction(async (input: EditConversationInput): Promise<ConversationWire | undefined> => {
    if (projectId === undefined) return undefined;
    return conversationEdit({ projectId, id: input.id, ...(input.name !== undefined ? { name: input.name } : {}), ...(input.isPrivate !== undefined ? { is_private: input.isPrivate } : {}) });
  });

  const remove = useAsyncAction(async (conversation: ConversationIdentity): Promise<boolean> => {
    if (conversation.isPlayback) return true;
    if (projectId === undefined) return false;
    await deleteConversationRequest({ projectId, id: conversation.id });
    return true;
  });

  const select = useAsyncAction(async (conversation: ConversationIdentity): Promise<ConversationWire | undefined> => {
    if (conversation.isPlayback || projectId === undefined) return undefined;
    const [details] = await Promise.all([
      conversationDetails({ projectId, id: conversation.id }),
      selectConversationRequest({ projectId, conversationId: conversation.id }),
    ]);
    return details;
  });

  const unselect = useAsyncAction(async (): Promise<void> => {
    if (projectId === undefined) return;
    await unselectConversationRequest({ projectId });
  });

  // `remove`/`unselect` are fresh `{run, isRunning, error}` object literals
  // every render (only their own `.run` is stabilized inside
  // useAsyncAction) — depending on the whole object here would reintroduce
  // the same instability these two useCallback wrappers exist to avoid, so
  // each `.run` is pulled into its own local first and that's what's
  // depended on.
  const removeRun = remove.run;
  const unselectRun = unselect.run;
  const deleteConversation = useCallback(async (conversation: ConversationIdentity): Promise<boolean> => (await removeRun(conversation)) ?? false, [removeRun]);
  const unselectConversation = useCallback(async (): Promise<void> => {
    await unselectRun();
  }, [unselectRun]);

  return {
    createConversation: create.run,
    isCreating: create.isRunning,
    createError: create.error,

    editConversation: edit.run,
    isEditing: edit.isRunning,
    editError: edit.error,

    deleteConversation,
    isDeleting: remove.isRunning,
    deleteError: remove.error,

    selectConversation: select.run,
    isSelecting: select.isRunning,
    selectError: select.error,

    unselectConversation,
  };
}
