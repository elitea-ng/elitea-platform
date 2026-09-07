import { useCallback, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';

import { useCreateApplicationDraft } from '@/entities/application-form';
import { LATEST_VERSION_NAME } from '@/entities/version';
import { getGetApplicationQueryOptions, getUpdateApplicationRelationQueryOptions } from '@/shared/api/generated/applications/applications';
import type { ApplicationCreatedResponse } from '@/shared/api/generated/model';
import { contextResolver } from '@/shared/lib/string';

import { mapAssociationError } from '../../lib/associationError';
import { applicationErrorMessage } from '../../lib/errorMessage';
import { filterEmptyStrings, type AgentDraft, type SuggestedResource } from '../../lib/agentDraft';

/**
 * The network-orchestration half of the baseline's `GenerateAgentModal.jsx`
 * `handleApprove` (lines 225-283), split into its own hook to keep
 * `GenerateAgentModal.tsx` under the §3.5 per-function complexity budget.
 *
 * **REAL, CONFIRMED BACKEND GAPS — toolkit/MCP/skill association dropped,
 * not silently ignored.** The baseline's `handleApprove` also calls
 * `associateToolkits`/`associateSkills` (`useToolkitAssociateMutation`,
 * `useUpdateSkillRelationMutation`, `useLazySkillDetailsQuery`). Grepped
 * the ENTIRE generated client (`shared/api/generated/**`) for `associate`/
 * `relation` on toolkits and skills: zero hits — no toolkit-association
 * endpoint and no skill-relation/skill-detail endpoint exists at all today
 * (beyond the already-documented "no toolkit-validation endpoint" gap).
 * Only agent/pipeline association survives, via the REAL
 * `useUpdateApplicationRelation` endpoint. This is not a functional loss
 * in practice today: `../../lib/agentDraft.ts`'s `mapApplicationDraft`
 * (the only draft source, and the endpoint it maps carries no resource
 * suggestions — see that file's own doc comment) always returns EMPTY
 * `suggested_toolkits`/`suggested_mcp`/`suggested_skills` arrays, so
 * `selectedToolkitIds`/`selectedMcpIds`/`selectedSkillIds` are always empty
 * sets at the one real call site (`GenerateAgentModal.tsx`) — there is
 * nothing to associate yet regardless.
 *
 *
 * **`llm_settings` — deliberately not authored here.** The generator's
 * `AgentDraft` describes an agent, not a model, so this flow has nothing to
 * pick from and omits the key: the new agent runs on the project's
 * catalogue default, which is what every version created before the model
 * picker existed does. (An earlier revision of this note blamed a missing
 * `ListModels` endpoint. That endpoint exists — `useListModelsQuery` in
 * `shared/api/configurationsApi.ts`, hand-written rather than generated,
 * which is why a grep confined to `shared/api/generated/` came back empty.)
 *
 * `agent_type` is deliberately left `undefined` (not `'openai'`, unlike
 * the baseline's explicit literal): `ApplicationVersionDraft.agentType`'s
 * type is `'pipeline' | undefined` (entities/application-form's own
 * create-flow contract has no slot for any OTHER literal), and
 * `VersionWriteRequest`'s generated doc comment states the Go handler
 * "Defaults to \"openai\" when empty" when the field is omitted — omitting
 * it here reaches the identical runtime outcome without widening a type
 * this sub-unit does not own.
 *
 * **Additional, newly-observed gap: `mapAssociationError`'s pattern-matched
 * branches are effectively unreachable through this exact wiring.** That
 * function (A1e, `../../lib/associationError.ts`) only names the entity
 * ("Cannot add \"Sub Agent\": ...") for RECOGNISED substrings (circular/
 * "uses other agents"/bind-itself) in the message it is given.
 * `applicationErrorMessage` (`../../lib/errorMessage.ts`) — the only error
 * adapter this sub-unit has to feed it — returns `EliteaApiError.message`,
 * a generic `"eliteaFetch: STATUS from URL"` string (verified by reading
 * `shared/api/generated/mutator.ts`'s `describeFailure`) that NEVER
 * contains the backend's actual JSON body text, so none of
 * `mapAssociationError`'s substring checks can ever match here — every
 * association failure surfaces as that generic, unnamed string. Confirmed
 * by this file's own test (`useAgentDraftApproval.test.tsx`, "warns...").
 * Not a bug introduced by this sub-unit: it is the necessary consequence of
 * composing two already-landed, real sibling primitives whose contracts
 * don't line up on this one axis, and neither is owned by this sub-unit.
 */

export interface UseAgentDraftApprovalParams {
  readonly projectId: string | undefined;
  /** Optional — falls back to `console.warn` per the established no-toast-system-yet convention (`features/notifications/lib/errorMessage.ts`'s own doc comment). */
  readonly onAssociationWarning?: ((message: string) => void) | undefined;
}

export interface UseAgentDraftApprovalResult {
  readonly approve: (draft: AgentDraft, selection: DraftSelection) => Promise<ApplicationCreatedResponse>;
  readonly isApproving: boolean;
}

export interface DraftSelection {
  readonly selectedAgentIds: ReadonlySet<number | string>;
  readonly selectedPipelineIds: ReadonlySet<number | string>;
}

function warn(message: string, onAssociationWarning: ((message: string) => void) | undefined): void {
  if (onAssociationWarning) onAssociationWarning(message);
  else console.warn(message);
}

export function useAgentDraftApproval({
  projectId,
  onAssociationWarning,
}: UseAgentDraftApprovalParams): UseAgentDraftApprovalResult {
  const queryClient = useQueryClient();
  const { create } = useCreateApplicationDraft(projectId);
  const [isApproving, setIsApproving] = useState(false);

  const associateOne = useCallback(
    async (versionId: number, entityId: number, candidate: SuggestedResource): Promise<void> => {
      if (projectId === undefined) return;
      try {
        const detailsResponse = await queryClient.query(
          getGetApplicationQueryOptions(projectId, Number(candidate.id)),
        );
        const details = (detailsResponse as { data: { version_details?: { id?: string | number } } }).data;
        const childVersionId = details.version_details?.id;
        if (childVersionId === undefined) return;

        await queryClient.query(
          getUpdateApplicationRelationQueryOptions(projectId, Number(candidate.id), Number(childVersionId), {
            application_id: entityId,
            version_id: versionId,
            has_relation: true,
          }),
        );
      } catch (error) {
        const entityLabel = candidate.agent_type === 'pipeline' ? 'pipeline' : 'agent';
        warn(
          mapAssociationError(applicationErrorMessage(error), candidate.name, { action: 'add', entityLabel }),
          onAssociationWarning,
        );
      }
    },
    [projectId, queryClient, onAssociationWarning],
  );

  const approve = useCallback(
    async (draft: AgentDraft, selection: DraftSelection): Promise<ApplicationCreatedResponse> => {
      setIsApproving(true);
      try {
        const result = await create({
          name: draft.name.trim(),
          description: draft.description,
          type: 'interface',
          version: {
            name: LATEST_VERSION_NAME,
            agentType: undefined,
            instructions: draft.instructions,
            // The generator drafts a welcome message and the review form
            // edits it; the draft carrying it end to end is what makes that
            // field real rather than decorative (the gap used to be in
            // `toVersionWriteRequest`, which had no key for it).
            welcomeMessage: draft.welcome_message,
            conversationStarters: filterEmptyStrings(draft.conversation_starters),
            variables: contextResolver(draft.instructions).map((name) => ({ name, value: '' })),
            // Empty `internal_tools` matches a fresh form: the gates admit
            // the whole authorable catalogue now (see
            // `internal_tools_catalogue_drift_test.go`), and a generated
            // agent starts with nothing toggled.
            meta: { step_limit: 25, internal_tools: [] },
            // See the "`llm_settings`" paragraph in this hook's doc comment:
            // absent, so the platform's catalogue-default fallback stands.
            llmSettings: undefined,
            tags: [],
            tools: [],
            pipelineSettings: undefined,
          },
        });
        if (!result) throw new Error('Failed to create the agent.');

        const entityId = Number(result.id);
        const versionId = result.version_details?.id !== undefined ? Number(result.version_details.id) : undefined;

        if (versionId !== undefined) {
          const selectedAgents = draft.suggested_agents.filter((a) => selection.selectedAgentIds.has(a.id));
          const selectedPipelines = draft.suggested_pipelines.filter((p) => selection.selectedPipelineIds.has(p.id));
          // `Promise.allSettled`-equivalent: every candidate's own `associateOne` already
          // catches and warns on its own failure (never rejects), so a plain `Promise.all`
          // runs them concurrently without one candidate's failure aborting the others —
          // same "surface, don't abort" semantics as the baseline's own `Promise.allSettled`.
          await Promise.all(
            [...selectedAgents, ...selectedPipelines].map((candidate) => associateOne(versionId, entityId, candidate)),
          );
        }

        return result;
      } finally {
        setIsApproving(false);
      }
    },
    [create, associateOne],
  );

  return { approve, isApproving };
}
