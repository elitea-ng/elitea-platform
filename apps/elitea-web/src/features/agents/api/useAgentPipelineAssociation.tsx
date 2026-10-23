import type { ReactNode } from 'react';
import { useCallback, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';
import type { QueryClient } from '@tanstack/react-query';

import { useTheme } from '@mui/material/styles';

import { getGetApplicationQueryOptions, getUpdateApplicationRelationQueryOptions } from '@/shared/api/generated/applications/applications';
import type { ApplicationDetail, ApplicationVersionDetail } from '@/shared/api/generated/model';
import { ApplicationsIcon } from '@/shared/ui/icons/applications-icon';
import { FlowIcon } from '@/shared/ui/icons/flow-icon';

import type { FilterableToolRef } from '../lib/useFilterAddedItems';
import { mapAssociationError } from '../lib/associationError';

/**
 * Ported from
 * `apps/elitea-ui/src/hooks/application/useAgentPipelineAssociation.jsx`'s
 * `useAgentPipelineAssociation` export (Wave-2 unit A1e; `mapAssociationError`
 * — the other export of that file — is ported separately as
 * `lib/associationError.ts`, reused here). Handles binding an existing
 * agent or pipeline as an `application`-typed "toolkit" tool on the version
 * currently being edited.
 *
 * **REAL BACKEND GAPS this port ran into (disclosed, not invented around):**
 *
 *  1. `updateApplicationRelation`'s response
 *     (`ApplicationRelationUpdatedResponse`, `shared/api/generated/model/
 *     applicationRelationUpdatedResponse.zod.ts`) is `{application_id,
 *     version_id, has_relation}` — an ECHO of the request, verified against
 *     the zod schema directly. There is no `tool_id` field at all, unlike
 *     the baseline's `result.tool_id`, which the baseline spliced straight
 *     into Formik's `version_details.tools` to render the new tool card
 *     immediately with no refetch. That optimistic-splice path has no
 *     faithful port: this hook cannot construct a `VersionToolRef`-shaped
 *     entry (no id, no `entity_type`, no persisted `settings`) from a
 *     response that never carried one. `associateAgent` therefore resolves
 *     `{ ok: true, message }` on success and does NOT attempt to hand back a
 *     constructed tool object; the caller (which owns the parent version's
 *     actual tools state — a react-hook-form field, a TanStack Query cache,
 *     or both) is expected to invalidate/refetch that version's detail
 *     query to pick up the real, server-assigned tool row — exactly the
 *     convention `entities/application-form/model/mutations.ts`'s own
 *     `useSaveApplicationVersion` doc comment already establishes for tool
 *     changes ("a caller that needs to change a version's tools must
 *     currently go through ... instead of one combined PUT").
 *  2. The baseline's swarm guard reads
 *     `selectedApplication.version_details.meta.internal_tools`. The
 *     generated `VersionMeta` schema (`shared/api/generated/model/
 *     versionMeta.zod.ts`) has NO `internal_tools` field at all — grepped
 *     the entire generated client for `internal_tools`, zero hits anywhere.
 *     `meta` is read here as a loosely-typed record and probed defensively
 *     (`meta?.['internal_tools']`) rather than through a typed field that
 *     does not exist, so the guard degrades gracefully (never blocks,
 *     since there is nothing to read) instead of failing to compile or
 *     silently inventing a field on `VersionMeta`. Flagged for whichever
 *     unit next touches the Go `ListTypeSchemas`/version-detail response
 *     shape — this is a real, observable capability loss (a swarm agent
 *     CAN currently be added as a tool through this UI, unlike the
 *     baseline), not a porting shortcut.
 *  3. No toast infrastructure exists in this app yet (see
 *     `features/mcps/model/useMcpAuthCheck.ts`'s own "`useToast` is
 *     replaced with an `onError` callback" precedent) — every baseline
 *     `toastError`/`toastSuccess` call becomes part of the resolved
 *     `AssociateAgentOutcome.message` instead; the caller decides how (or
 *     whether) to surface it.
 *
 * `associateAgent`'s guard checks (swarm/container/duplicate) are split into
 * their own functions purely to stay under the §3.5 cyclomatic-complexity
 * budget (12) — the baseline's inline shape had a complexity of 19.
 * `getToolIcon`'s hard-coded `fill="#FFFFFF"` becomes
 * `theme.vars.palette.icon.fill.button` (R-T1: no raw colour literals —
 * this is the closest semantic match in `palette.augment.d.ts`'s
 * `icon.fill.*` vocabulary for "an icon rendered on a filled/coloured
 * surface", the exact context both baseline icons render in).
 */

export interface AssociationCandidate {
  readonly id: number;
  readonly name: string;
}

interface AssociateAgentOutcome {
  readonly ok: boolean;
  /** Always a caller-displayable message, on both success and failure — see module doc comment, gap 3. */
  readonly message: string;
}

interface AssociateAgentOptions {
  readonly isPipeline?: boolean;
  /** The parent version's CURRENT tools, for the client-side duplicate guard (defense in depth — the caller's own dropdown filtering, e.g. `useFilterAddedItems`, is the primary guard). */
  readonly currentTools?: readonly FilterableToolRef[];
}

export interface UseAgentPipelineAssociationParams {
  readonly projectId: string | undefined;
  /** The PARENT application/pipeline currently being edited. */
  readonly applicationId: number | undefined;
  /** The PARENT version currently being edited. */
  readonly versionId: number | undefined;
}

export interface UseAgentPipelineAssociationResult {
  readonly associateAgent: (candidate: AssociationCandidate, options?: AssociateAgentOptions) => Promise<AssociateAgentOutcome>;
  readonly getToolIcon: (toolType: string | undefined, size?: string) => ReactNode;
  readonly isAssociating: boolean;
}

/** Loosely-typed probe into `meta` for a field the generated `VersionMeta` schema does not declare — see module doc comment, gap 2. */
function readInternalTools(meta: unknown): readonly unknown[] {
  if (typeof meta !== 'object' || meta === null) return [];
  const internalTools = (meta as Record<string, unknown>)['internal_tools'];
  return Array.isArray(internalTools) ? internalTools : [];
}

function extractHttpErrorBody(error: unknown): unknown {
  if (error instanceof Error && 'failure' in error) {
    const failure = (error as { failure?: unknown }).failure;
    if (typeof failure === 'object' && failure !== null && (failure as { kind?: unknown }).kind === 'http') {
      return (failure as { body?: unknown }).body;
    }
  }
  return error;
}

/** Pulls a backend `{"error": "..."}`-shaped message out of an HTTP failure body (see the applications.ts generated client's own documented error-shape convention), falling back to the raw body/error. */
function extractBackendErrorMessage(error: unknown): unknown {
  const body = extractHttpErrorBody(error);
  if (typeof body === 'object' && body !== null && 'error' in body) {
    return (body as { error?: unknown }).error;
  }
  return body;
}

/** Block swarm agents from being added as tools (see module doc comment, gap 2 — always `[]` against the real backend today). */
function checkSwarmGuard(candidateVersion: ApplicationVersionDetail | undefined, candidateName: string): AssociateAgentOutcome | null {
  if (!readInternalTools(candidateVersion?.meta).includes('swarm')) return null;
  return {
    ok: false,
    message: `"${candidateName}" has Swarm Mode enabled and cannot be added as a tool. Swarm agents are only supported in direct chat.`,
  };
}

/**
 * Block "container" agents from being nested (issue #5680). A non-pipeline agent that itself
 * uses other agents (has an 'application'-type tool) may only run at the top as a direct chat
 * participant — nesting it would create an unsupported extra nesting level. Pipelines are the
 * sanctioned deep-composition primitive and are exempt from this rule.
 */
function checkContainerGuard(
  candidateVersion: ApplicationVersionDetail | undefined,
  candidateName: string,
  entityLabel: 'agent' | 'pipeline',
): AssociateAgentOutcome | null {
  const candidateIsContainer = (candidateVersion?.tools ?? []).some((tool) => tool.type === 'application');
  if (candidateVersion?.agent_type === 'pipeline' || !candidateIsContainer) return null;
  return {
    ok: false,
    message: mapAssociationError('uses other agents and cannot be added as a sub-agent', candidateName, { action: 'add', entityLabel }),
  };
}

function checkDuplicateGuard(
  currentTools: readonly FilterableToolRef[],
  selectedApplicationId: string,
  candidateName: string,
  entityLabel: 'agent' | 'pipeline',
): AssociateAgentOutcome | null {
  const existingTool = currentTools.find(
    (tool) => tool.type === 'application' && String(tool.settings?.application_id) === selectedApplicationId,
  );
  if (!existingTool) return null;
  return { ok: false, message: `The "${candidateName}" ${entityLabel} is already added to this ${entityLabel}.` };
}

interface PerformAssociationParams {
  readonly queryClient: QueryClient;
  readonly projectId: string;
  readonly applicationId: number;
  readonly versionId: number;
  readonly candidate: AssociationCandidate;
  readonly entityLabel: 'agent' | 'pipeline';
  readonly currentTools: readonly FilterableToolRef[];
}

/**
 * The network half of `associateAgent` — fetch the candidate's own detail, run the guard
 * checks, then issue the relation update. Split out of `associateAgent` itself purely to
 * stay under the §3.5 cyclomatic-complexity budget (12).
 */
async function performAssociation({ queryClient, projectId, applicationId, versionId, candidate, entityLabel, currentTools }: PerformAssociationParams): Promise<AssociateAgentOutcome> {
  const detailResponse = await queryClient.query(getGetApplicationQueryOptions(projectId, candidate.id));
  // Same error-envelope-unreachable cast as `entities/application-form/model/mutations.ts` (eliteaFetch throws instead of resolving with the error variant).
  const selectedApplication = (detailResponse as { data: ApplicationDetail }).data;
  const candidateVersion = selectedApplication.version_details;

  const guardFailure =
    checkSwarmGuard(candidateVersion, candidate.name) ??
    checkContainerGuard(candidateVersion, candidate.name, entityLabel) ??
    checkDuplicateGuard(currentTools, selectedApplication.id, candidate.name, entityLabel);
  if (guardFailure) return guardFailure;

  if (candidateVersion?.id === undefined) {
    return { ok: false, message: `"${candidate.name}" has no version to attach.` };
  }

  // A write is never retried: a refused attach (400) went out four times.
  await queryClient.query({
    ...getUpdateApplicationRelationQueryOptions(projectId, Number(selectedApplication.id), Number(candidateVersion.id), {
      application_id: applicationId,
      version_id: versionId,
      has_relation: true,
    }),
    retry: false,
  });

  return { ok: true, message: `The "${candidate.name}" ${entityLabel} is successfully added.` };
}

export function useAgentPipelineAssociation({
  projectId,
  applicationId,
  versionId,
}: UseAgentPipelineAssociationParams): UseAgentPipelineAssociationResult {
  const queryClient = useQueryClient();
  const theme = useTheme();
  const [isAssociating, setIsAssociating] = useState(false);

  const associateAgent = useCallback(
    async (candidate: AssociationCandidate, options: AssociateAgentOptions = {}): Promise<AssociateAgentOutcome> => {
      const { isPipeline = false, currentTools = [] } = options;
      const entityLabel = isPipeline ? 'pipeline' : 'agent';

      if (projectId === undefined || applicationId === undefined || versionId === undefined) {
        return { ok: false, message: 'Application ID and Version ID are required to associate agent' };
      }

      setIsAssociating(true);
      try {
        return await performAssociation({ queryClient, projectId, applicationId, versionId, candidate, entityLabel, currentTools });
      } catch (error) {
        return { ok: false, message: mapAssociationError(extractBackendErrorMessage(error), candidate.name, { action: 'add', entityLabel }) };
      } finally {
        setIsAssociating(false);
      }
    },
    [applicationId, projectId, queryClient, versionId],
  );

  const getToolIcon = useCallback(
    (toolType: string | undefined, size = '16px'): ReactNode => {
      const fill = theme.vars.palette.icon.fill.button;
      if (toolType === 'agent') {
        return (
          <ApplicationsIcon
            width={size}
            height={size}
            fill={fill}
          />
        );
      }
      if (toolType === 'pipeline') {
        return (
          <FlowIcon
            width={size}
            height={size}
            fill={fill}
          />
        );
      }
      return null;
    },
    [theme],
  );

  return { associateAgent, getToolIcon, isAssociating };
}
