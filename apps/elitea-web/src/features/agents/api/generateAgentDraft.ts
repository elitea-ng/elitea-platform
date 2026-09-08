import { useCallback, useState } from 'react';

import { eliteaFetch } from '@/shared/api/generated/mutator';
import type { ApplicationDraft } from '@/shared/api/generated/model';

/**
 * Ported from
 * `apps/elitea-ui/src/[fsd]/features/agent/api/generateAgentDraftApi.js` (a
 * hand-built RTK Query `injectEndpoints` mutation, `POST /elitea_core/
 * generate_application_draft/prompt_lib/{projectId}`).
 *
 * The two shape gaps this module used to disclose are CLOSED (#254 P1). The
 * route is served by `internal/api/v2/drafts` and described in
 * `api/openapi/v2.yaml` as `generateApplicationDraft`, so:
 *
 *  1. **Request body.** `{ user_description }` is the field the endpoint reads,
 *     the same one the baseline sends. The previous `user_description -> input`
 *     mapping existed only because the URL then reached the GENERIC predictor,
 *     which has no such field. It is gone.
 *  2. **Response body.** `ApplicationDraft` — `{name, description,
 *     instructions, welcome_message, conversation_starters}` — is a structured
 *     draft, not a chat-completion envelope. `mapApplicationDraft` in
 *     `../lib/agentDraft` turns it into the `AgentDraft` the review form edits.
 *
 * ONE GAP REMAINS, and it is a backend one rather than a client mapping: the
 * contract carries no `suggested_toolkits` / `suggested_mcp` /
 * `suggested_agents` / `suggested_pipelines` / `suggested_skills`. Legacy
 * builds those from a project-resource inventory elitea-main composes no
 * reader for; see the `ApplicationDraft` schema description. `AgentDraft`
 * still declares all five and `mapApplicationDraft` leaves them empty, so
 * `ResourceSuggestions` renders nothing for them — the same graceful
 * degradation as before, now for a reason recorded in the contract.
 *
 * The call still goes through `eliteaFetch` rather than the generated hook:
 * orval shapes every write endpoint as a `useQuery` gated by `enabled`
 * (see `entities/application-form/model/mutations.ts`), and this affordance is
 * a "click to generate" button. The URL, method and body below are the
 * generated operation's, and the manifest entry carries its `operationId` so
 * the spec/router conformance gate matches it.
 */
export interface UseGenerateAgentDraftMutationArgs {
  readonly projectId: string;
  readonly user_description: string;
}

export interface UseGenerateAgentDraftMutationResult {
  readonly generateDraft: (args: UseGenerateAgentDraftMutationArgs) => Promise<ApplicationDraft | undefined>;
  readonly isLoading: boolean;
  readonly error: unknown;
  readonly reset: () => void;
}

/**
 * Imperative "generate an AI agent draft" action. Named `useGenerateAgentDraftMutation`
 * (not `useGenerateAgentDraft`) to match the baseline's own RTK Query hook
 * name — this app's generated client already exports an UNRELATED
 * `useGenerateAgentDraft` (a `useQuery`-shaped hook gated by `enabled`, per
 * this generated client's convention for every write endpoint — see
 * `entities/application-form/model/mutations.ts`'s doc comment); this
 * wrapper is the imperative, mutation-shaped call site callers actually
 * need (a "click to generate" button, not an auto-firing query).
 */
export function useGenerateAgentDraftMutation(): UseGenerateAgentDraftMutationResult {
  const [isLoading, setIsLoading] = useState(false);
  const [error, setError] = useState<unknown>(undefined);

  const generateDraft = useCallback(
    async ({ projectId, user_description }: UseGenerateAgentDraftMutationArgs): Promise<ApplicationDraft | undefined> => {
      setIsLoading(true);
      setError(undefined);
      try {
        // Error-envelope response variants (400/401/403) are never actually reachable here —
        // `eliteaFetch` throws `EliteaApiError` instead of resolving with them (mutator.ts's
        // §3.6 unwrap contract; same cast convention as `entities/application-form`'s hooks).
        const response = await eliteaFetch<{ data: ApplicationDraft }>(
          `/elitea_core/generate_application_draft/prompt_lib/${projectId}`,
          {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ user_description }),
          },
        );
        return response.data;
      } catch (caught) {
        setError(caught);
        return undefined;
      } finally {
        setIsLoading(false);
      }
    },
    [],
  );

  const reset = useCallback(() => {
    setError(undefined);
  }, []);

  return { generateDraft, isLoading, error, reset };
}
