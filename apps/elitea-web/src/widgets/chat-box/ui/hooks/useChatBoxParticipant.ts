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
import { grantableTools, type SensitiveToolRef } from '@/features/unattended';
import { useParticipantName } from '@/features/chat-messages';

import { toParticipant, toParticipants } from '../ChatBox.helpers';
import { useChatBoxAttachmentsGate } from './useChatBoxAttachmentsGate';
import type { ResolvedTarget } from './useChatBoxSend.floor';

interface AgentEditorParticipantDetails {
  readonly id?: string;
  readonly name?: string;
  readonly versions?: ReturnType<typeof normaliseVersionSummaries>;
}

/**
 * What the agent's open version says to a new conversation (#1987, #1988): its
 * welcome message and conversation starters, off the version the participant is
 * bound to. A pure read of the already-fetched participant details, so it is
 * exported for its test and not for callers.
 */
export function readAgentGreeting(raw: Record<string, unknown> | undefined): {
  readonly welcomeMessage: string | undefined;
  readonly starters: readonly string[] | undefined;
} {
  const version = raw?.['version_details'];
  if (typeof version !== 'object' || version === null) return { welcomeMessage: undefined, starters: undefined };
  const record = version as Record<string, unknown>;
  const welcome = typeof record['welcome_message'] === 'string' ? record['welcome_message'].trim() : '';
  const rawStarters = record['conversation_starters'];
  const starters = Array.isArray(rawStarters)
    ? rawStarters.filter((entry): entry is string => typeof entry === 'string' && entry.trim() !== '')
    : undefined;
  return { welcomeMessage: welcome === '' ? undefined : welcome, starters };
}

export interface UseChatBoxParticipantParams {
  readonly activeParticipant: unknown;
  readonly conversationParticipants: unknown[] | undefined;
  /** The participant the send goes to (`useResolvedTarget`), for the attachments gate. */
  readonly target: ResolvedTarget;
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
  /** The bound agent version's welcome message, shown in an empty conversation (#1987). */
  readonly welcomeMessage: string | undefined;
  /** The bound agent version's conversation starters (#1988); the caller's own prop wins when it has one. */
  readonly participantStarters: readonly string[] | undefined;
  /** The tools of the target agent's version (the active one in the editor) that an unattended grant can name (ADR-0045); undefined without an agent. */
  readonly grantableTools: readonly SensitiveToolRef[] | undefined;
}

export function useChatBoxParticipant({
  activeParticipant,
  conversationParticipants,
  target,
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

  const greeting = useMemo(() => readAgentGreeting(rawParticipantDetails), [rawParticipantDetails]);

  const gate = useChatBoxAttachmentsGate({ target, isAgentsPage });
  const areAttachmentsGated = gate.gated;
  // The tools the grant names are the TARGET's (the agent the send goes to); the editor surface keeps its own participant's.
  const versionDetails = isAgentsPage ? (participantForEditor === undefined ? undefined : rawParticipantDetails['version_details']) : gate.versionDetails;
  const tools = useMemo(() => (versionDetails === undefined ? undefined : grantableTools(versionDetails)), [versionDetails]);

  return {
    participantForEditor,
    normalisedParticipants,
    agentEditorParticipantDetails,
    isFetchingParticipantDetails,
    assistantName,
    areAttachmentsGated,
    welcomeMessage: greeting.welcomeMessage,
    participantStarters: greeting.starters,
    grantableTools: tools,
  };
}
