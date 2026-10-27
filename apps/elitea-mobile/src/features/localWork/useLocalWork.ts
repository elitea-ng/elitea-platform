import { useEffect, useRef, useState } from 'react';
import { Alert } from 'react-native';
import { useQueryClient } from '@tanstack/react-query';

import { speaksContract } from '@/api/discovery';
import { isApiError } from '@/api/errors';
import { queryKeys } from '@/api/queries/keys';
import { eraseCloudCopy, type HandoffInfo, requestReclaim, useHandoffInfo, usePermissionToEditProjectSettings } from '@/api/queries/workspaceHandoff';
import type { ConversationDetail } from '@/api/queries/conversation';
import {
  canEraseCloudCopy,
  canRequestReclaim,
  composerBlockedReason,
  describeLocalWork,
  executorOf,
  isLocalWork,
  executorOfState,
  FORGE_CREDENTIAL_CONTRACT,
  HANDOFF_CONTRACT,
  type LocalWorkThread,
} from '@/features/localWork/model';
import { credentialButtonTitle } from '@/features/localWork/forgeCredential';
import { useWorkspaceInfo } from '@/state/workspaceInfo';
import { useOnline } from '@/lib/network';

export interface LocalWorkState {
  /** Null for an ordinary chat. */
  thread: LocalWorkThread | null;
  /** Why the composer is off, or null (a chat, or a thread the cloud runs). */
  blockedReason: string | null;
  canTakeBack: boolean;
  canErase: boolean;
  takeBackRequested: boolean;
  requesting: boolean;
  erasing: boolean;
  takeBack: () => void;
  erase: () => void;
  /** The owner's control over the cloud's push and pull-request credential (contract 1.29); null hides it. */
  credentialTitle: string | null;
  /** The thread's uuid, for the credential sheet. */
  uuid: string | null;
}

const REFUSED: Record<number, string> = {
  403: 'You cannot do that for this thread.',
  404: 'This thread is no longer in the cloud.',
};

function failureText(error: unknown, fallback: string): string {
  return isApiError(error) && error.kind === 'http' && error.status !== undefined && REFUSED[error.status] ? REFUSED[error.status] : fallback;
}

/**
 * The thread screen's Local work state: where the thread runs (the conversation's
 * `workspace_executor`, kept live by polling the handoff state while the screen is open), who
 * may do what, and the two cloud actions. An ordinary chat gets an inert state.
 *
 * `notify` shows a short confirmation; `fail` shows an error note.
 */
export function useLocalWork({
  projectId,
  conversationRef,
  detail,
  offlineCopy,
  selfUserId,
  notify,
  fail,
}: {
  projectId: number | null;
  /** The route's conversation ref (the key of the conversation query). */
  conversationRef: string;
  detail: ConversationDetail | undefined;
  offlineCopy: boolean;
  selfUserId: number | null;
  notify: (text: string) => void;
  fail: (text: string) => void;
}): LocalWorkState {
  const queryClient = useQueryClient();
  const workspace = useWorkspaceInfo();
  const online = useOnline();
  const uuid = typeof detail?.uuid === 'string' && detail.uuid !== '' ? detail.uuid : null;
  const speaksHandoff = speaksContract(workspace?.discovery, HANDOFF_CONTRACT.major, HANDOFF_CONTRACT.minor);
  const local = isLocalWork(detail);
  const live = useHandoffInfo(projectId, uuid, local && speaksHandoff && online && !offlineCopy);
  // The handoff read is the fresher of the two; the conversation's own field until it answers.
  const detailExecutor = executorOf(detail);
  const fresh = executorOfState(live.data?.state);
  const executor = fresh ?? detailExecutor;
  const thread = local ? describeLocalWork(detail, executor, selfUserId) : null;

  // The thread moved without this screen doing anything (the laptop took it back, an erase
  // elsewhere): read the conversation again, once per change, so the transcript and composer
  // follow. A conversation that does not carry the field (an older read) is never re-read for it.
  const hasField = typeof (detail as { workspace_executor?: unknown } | undefined)?.workspace_executor === 'string';
  const seen = useRef<string | null>(null);
  useEffect(() => {
    if (projectId === null || fresh === null || !local) return;
    const before = seen.current ?? (hasField ? detailExecutor : null);
    seen.current = fresh;
    if (before !== null && before !== fresh) void queryClient.invalidateQueries({ queryKey: queryKeys.conversation(projectId, conversationRef) });
  }, [fresh, local, hasField, detailExecutor, projectId, conversationRef, queryClient]);

  const canEditSettings = usePermissionToEditProjectSettings(projectId, thread !== null && thread.executor !== 'local' && !thread.isOwner && online && !offlineCopy);
  const canTakeBack = thread !== null && canRequestReclaim(thread) && online && !offlineCopy;
  const canErase = thread !== null && canEraseCloudCopy(thread, canEditSettings) && online && !offlineCopy;
  // The credential is the owner's choice, made while the cloud runs the thread, on a 1.29 server.
  const speaksForge = speaksContract(workspace?.discovery, FORGE_CREDENTIAL_CONTRACT.major, FORGE_CREDENTIAL_CONTRACT.minor);
  const canChooseCredential = thread !== null && thread.executor === 'cloud' && thread.isOwner && speaksForge && online && !offlineCopy && uuid !== null;

  const [requesting, setRequesting] = useState(false);
  const [requestedHere, setRequestedHere] = useState(false);
  const [erasing, setErasing] = useState(false);

  const takeBack = () => {
    if (projectId === null || uuid === null || requesting) return;
    setRequesting(true);
    requestReclaim(projectId, uuid)
      .then(() => {
        setRequestedHere(true);
        notify('Sent to your laptop');
      })
      .catch((error: unknown) => fail(failureText(error, "The request couldn't be sent. Try again.")))
      .finally(() => setRequesting(false));
  };

  const doErase = () => {
    if (projectId === null || uuid === null || erasing) return;
    setErasing(true);
    eraseCloudCopy(projectId, uuid)
      .then(() => {
        notify('The cloud copy was erased');
        // Back on the laptop at once; the poll confirms (LOCAL always replaces the cloud state).
        queryClient.setQueryData<HandoffInfo>(queryKeys.handoff(projectId, uuid), { state: 'LOCAL', epoch: 0 });
        // The thread is the laptop's again: read the conversation, its handoff and the lists afresh.
        void queryClient.invalidateQueries({ queryKey: queryKeys.conversation(projectId, conversationRef) });
        void queryClient.invalidateQueries({ queryKey: queryKeys.handoff(projectId, uuid) });
        void queryClient.invalidateQueries({ queryKey: queryKeys.localWork(projectId) });
      })
      .catch((error: unknown) => fail(failureText(error, "The cloud copy couldn't be erased. Try again.")))
      .finally(() => setErasing(false));
  };

  const erase = () => {
    Alert.alert(
      'Erase the cloud copy?',
      'The files in the cloud, and the changes the cloud made to them, are deleted, and a turn that is running is stopped. The thread goes back to the laptop it came from.',
      [
        { text: 'Keep it', style: 'cancel' },
        { text: 'Erase cloud copy', style: 'destructive', onPress: doErase },
      ],
      { cancelable: true },
    );
  };

  return {
    thread,
    blockedReason: composerBlockedReason(thread),
    canTakeBack,
    canErase,
    takeBackRequested: requestedHere || live.data?.reclaim_pending === true,
    requesting,
    erasing,
    takeBack,
    erase,
    credentialTitle: canChooseCredential ? credentialButtonTitle(live.data?.pull_request_credential) : null,
    uuid,
  };
}
