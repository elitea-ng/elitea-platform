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

interface AgentEditorParticipantDetails {
  readonly id?: string;
  readonly name?: string;
  readonly versions?: ReturnType<typeof normaliseVersionSummaries>;
}

export interface UseChatBoxParticipantParams {
  readonly activeParticipant: unknown;
  readonly conversationParticipants: unknown[] | undefined;
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
}

export function useChatBoxParticipant({
  activeParticipant,
  conversationParticipants,
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

  return { participantForEditor, normalisedParticipants, agentEditorParticipantDetails, isFetchingParticipantDetails, assistantName };
}
