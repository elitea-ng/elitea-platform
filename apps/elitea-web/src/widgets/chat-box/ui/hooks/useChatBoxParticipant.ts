/**
 * Split out of `ChatBox.tsx` to stay under the file-length/component-props
 * budgets (§3.5) — normalises the raw wire-shape `activeParticipant`/
 * `participants` props into `entities/participant`'s `Participant` shape,
 * and fetches the active participant's full details (versions, etc.),
 * matching baseline's `originalParticipant` effect (`ChatBox.jsx:1684-1750`).
 */
import { useMemo } from 'react';

import type { Participant } from '@/entities/participant';
import { normaliseVersionSummaries } from '@/entities/version';
import { useActiveParticipantDetails } from '@/features/chat-participants';
import { useParticipantName } from '@/features/chat-messages';

import { toParticipant, toParticipants } from '../ChatBox.helpers';
import { useChatBoxAttachmentsGate } from './useChatBoxAttachmentsGate';

interface AgentEditorParticipantDetails {
  readonly id?: string;
  readonly name?: string;
  readonly versions?: ReturnType<typeof normaliseVersionSummaries>;
}

export interface UseChatBoxParticipantParams {
  readonly activeParticipant: unknown;
  readonly conversationParticipants: unknown[] | undefined;
  /** #905: the agent/pipeline EDITOR surface computes the attachments gate from its own (unsaved) form state — the gate below stands down there. */
  readonly isAgentsPage: boolean | undefined;
}

export interface UseChatBoxParticipantResult {
  readonly participantForEditor: Participant | undefined;
  readonly normalisedParticipants: Participant[] | undefined;
  readonly agentEditorParticipantDetails: AgentEditorParticipantDetails | undefined;
  readonly isFetchingParticipantDetails: boolean;
  /**
   * The active participant's display name, captioned on every assistant
   * transcript row (baseline `ApplicationAnswer.jsx`'s own
   * `useParticipantName(participant)`). Falls back to the deployment's
   * `system_sender_name` — "Elitea" by default — when the conversation has no
   * named participant, which is the plain-model case.
   */
  readonly assistantName: string;
  /**
   * #905: whether the ACTIVE agent/pipeline participant's "Allow attachments"
   * toggle is off. The caller ORs it with the in-flight-run refusal (A17,
   * ELITEA-2867) to get the composer's one `disableAttachments` flag, which
   * covers every way a file gets in: the "+" menu's Attach Files row, the bare
   * paperclip, and the drop/paste bridge.
   */
  readonly areAttachmentsGated: boolean;
}

export function useChatBoxParticipant({
  activeParticipant,
  conversationParticipants,
  isAgentsPage,
}: UseChatBoxParticipantParams): UseChatBoxParticipantResult {
  const participantForEditor = useMemo(() => toParticipant(activeParticipant), [activeParticipant]);
  const assistantName = useParticipantName(activeParticipant as Parameters<typeof useParticipantName>[0]);
  const normalisedParticipants = useMemo(
    () => toParticipants(conversationParticipants),
    [conversationParticipants],
  );
  const { activeParticipantDetails: rawParticipantDetails, isLoadingDetails: isFetchingParticipantDetails } = useActiveParticipantDetails({
    activeParticipant: (activeParticipant ?? null) as Record<string, unknown> | null,
  });
  const agentEditorParticipantDetails = useMemo(() => {
    const raw = rawParticipantDetails;
    if (!raw || Object.keys(raw).length === 0) return undefined;
    const id = typeof raw['id'] === 'string' ? raw['id'] : undefined;
    const name = typeof raw['name'] === 'string' ? raw['name'] : undefined;
    const rawVersions = raw['versions'];
    const versions = Array.isArray(rawVersions)
      ? normaliseVersionSummaries(rawVersions as Parameters<typeof normaliseVersionSummaries>[0])
      : undefined;
    return { ...(id !== undefined ? { id } : {}), ...(name !== undefined ? { name } : {}), ...(versions !== undefined ? { versions } : {}) };
  }, [rawParticipantDetails]);

  const areAttachmentsGated = useChatBoxAttachmentsGate({ activeParticipant, participants: conversationParticipants, isAgentsPage });

  return { participantForEditor, normalisedParticipants, agentEditorParticipantDetails, isFetchingParticipantDetails, assistantName, areAttachmentsGated };
}
