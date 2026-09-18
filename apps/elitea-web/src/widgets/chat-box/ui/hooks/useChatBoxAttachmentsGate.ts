/**
 * The agent's "Allow attachments" gate on the MAIN chat page (issue #905,
 * ELITEA-0509).
 *
 * The gate itself is `entities/application-form`'s `isAttachmentsEnabled` —
 * the same predicate the Agent/Pipeline editor's own embedded test-chat runs
 * through `features/agents/lib/useAgentAttachments.ts`. What this hook adds
 * is WHICH participant it is asked about.
 *
 * It is not the one the composer's editor panel shows. A cold deep link opens
 * with no active participant at all (`useActiveParticipantSelection`'s own
 * doc comment), yet the turn still reaches the conversation's single
 * application participant — `useChatBoxSend.helpers`' `resolveTargetParticipant`
 * is what makes that true. Gating on the ACTIVE participant would therefore
 * leave attachments enabled for exactly the case the issue describes:
 * attach an agent, open the conversation, open the "+" menu. The gate uses
 * the same resolution the SEND path uses, so the control reflects the agent
 * that will actually receive the files.
 */
import { useMemo } from 'react';

import { isAttachmentsEnabled } from '@/entities/application-form';
import type { Participant } from '@/entities/participant';
import { useActiveParticipantDetails } from '@/features/chat-participants';

import { toParticipant } from '../ChatBox.helpers';
import { resolveTargetParticipant } from './useChatBoxSend.helpers';

/** `version_details.meta.internal_tools` off a raw participant-details object, or `undefined` when it is absent/not a list of names. */
function readInternalTools(raw: Record<string, unknown> | undefined): readonly string[] | undefined {
  const versionDetails = raw?.['version_details'] as Record<string, unknown> | undefined;
  const meta = versionDetails?.['meta'] as Record<string, unknown> | undefined;
  const tools = meta?.['internal_tools'];
  if (!Array.isArray(tools)) return undefined;
  return tools.filter((tool): tool is string => typeof tool === 'string');
}

/** Only agent/pipeline rows have a version whose `meta.internal_tools` could carry the toggle — and only those two are types `useActiveParticipantDetails` can fetch at all. */
function isVersionedEntity(participant: unknown): boolean {
  const entityName = (participant as { readonly entity_name?: unknown } | null | undefined)?.entity_name;
  return entityName === 'application' || entityName === 'pipeline';
}

/**
 * The gate itself, over a NORMALISED participant. `isAttachmentsEnabled`
 * (`entities/application-form`) is the same predicate
 * `features/agents/lib/useAgentAttachments.ts` runs in the agent editor's own
 * test-chat. Scoped to agent/pipeline participants: a plain-model
 * conversation, a toolkit/MCP participant, or none at all keeps attachments
 * enabled. `undefined` internal tools (details not resolved yet, or a version
 * with no `meta`) read as OFF — the same conservative default the editor's
 * gate applies.
 */
export function shouldDisableParticipantAttachments(
  participant: Participant | undefined,
  internalTools: readonly string[] | undefined,
): boolean {
  const entityName = participant?.entityName;
  if (entityName !== 'application' && entityName !== 'pipeline') return false;
  return !isAttachmentsEnabled(internalTools);
}

export interface UseChatBoxAttachmentsGateParams {
  readonly activeParticipant: unknown;
  readonly participants: readonly unknown[] | undefined;
  /** The agent/pipeline EDITOR surface computes this gate from its own (unsaved) form state — this hook stands down there. */
  readonly isAgentsPage: boolean | undefined;
}

export function useChatBoxAttachmentsGate({ activeParticipant, participants, isAgentsPage }: UseChatBoxAttachmentsGateParams): boolean {
  const target = useMemo(
    () => (isAgentsPage ? undefined : resolveTargetParticipant(activeParticipant, participants)),
    [isAgentsPage, activeParticipant, participants],
  );
  const gated = isVersionedEntity(target);
  // `null` when the target is not a versioned entity: the details hook throws
  // `assertNever` for a toolkit/MCP/user row, and there is nothing to gate on
  // for a plain-model conversation anyway.
  const { activeParticipantDetails } = useActiveParticipantDetails({
    activeParticipant: gated ? (target as Record<string, unknown>) : null,
  });
  const internalTools = useMemo(() => readInternalTools(activeParticipantDetails), [activeParticipantDetails]);

  return shouldDisableParticipantAttachments(toParticipant(target), internalTools);
}
