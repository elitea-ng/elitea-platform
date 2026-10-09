import type { ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import { useQueryClient } from '@tanstack/react-query';
import { useNavigate, useSearch } from '@tanstack/react-router';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { isMcpToolkit } from '@/entities/toolkit';
import type { Toolkit } from '@/entities/toolkit';
import { getGetApplicationQueryKey, useGetApplication, useListApplications } from '@/shared/api/generated/applications/applications';
import type { Application, ApplicationDetail } from '@/shared/api/generated/model';
import { getToolkit } from '@/shared/api/generated/toolkits/toolkits';
import { unwrapBody, unwrapList } from '@/shared/api/unwrap';
import { t } from '@/shared/i18n';
import { SearchParams } from '@/shared/lib/params';

import { useAgentPipelineAssociation } from '../api/useAgentPipelineAssociation';
import type { AssociationCandidate } from '../api/useAgentPipelineAssociation';
import { useIsMcpVisible } from '../api/useIsMcpVisible';
import { useSelectedProjectId } from '../api/useSelectedProjectId';
import { setToolkitRelation } from '../lib/toolRelation';
import { useFilterAddedItems } from '../lib/useFilterAddedItems';
import type { AgentToolAssociation } from '../lib/types';

import { EntityAddSection, InstanceAddSection, useEntityAssociationItems } from './ToolMenuSections';

/**
 * Ported from
 * `apps/elitea-ui/src/pages/Applications/Components/Tools/ToolMenu.jsx`
 * (Wave-2 unit A1e). Renders the "Toolkit / MCP / Agent / Pipeline" add
 * buttons above an agent or pipeline's tool list, each opening a searchable
 * dropdown. The actual button+dropdown compositions live in
 * `ToolMenuSections.tsx` (split out to stay under the §3.5 400-line-per-file
 * budget) — this file owns only the entity-detail resolution (`useToolMenuEntityDetail`) and top-level wiring.
 *
 * **Public contract fixed by an already-landed sibling caller.**
 * `features/agents/ui/ApplicationTools.tsx` (sibling sub-unit, already in
 * this worktree) renders `<ToolMenu applicationId={applicationId} />` with
 * ONLY an `applicationId` prop — no `versionId`, no Formik context. This
 * component therefore resolves its own `versionId`/`agent_type`/current
 * `tools` by fetching the application's own detail via `useGetApplication`
 * (`useToolMenuEntityDetail` below), rather than requiring them as props the
 * way the baseline pulled them from `useFormikContext()`. `onToolsChanged`
 * is an ADDITIONAL, optional prop (not on `ApplicationTools`' current call
 * site) for a future caller that DOES own a separate `tools` list (e.g. the
 * real `ApplicationTools`'s own `tools` prop) to know when to refetch it —
 * see the "known integration gap" paragraph below.
 *
 * **REAL GAPS, disclosed (not invented around):**
 *  - No ORVAL-GENERATED wrapper exists for the toolkit-association endpoint
 *    (grepped `shared/api/generated/toolkits/toolkits.ts` — only
 *    `listToolkits`/`listToolkitInstances`). The endpoint itself IS real,
 *    though: `services/elitea-main/internal/api/router.go`'s toolkit-instance
 *    route group wires `r.Patch("/tool/prompt_lib/{projectID}/{toolkitID}",
 *    toolkitHandler.Update)`, and `internal/api/v2/toolkits/handler.go`'s
 *    `Update`/`updateToolRelation` inserts a row into `entity_tool_mapping`
 *    and returns `201 {"message": "ok"}` (no `tool_id` — the same "echo, no
 *    constructed row" response shape `useAgentPipelineAssociation.tsx`'s own
 *    doc comment documents for `updateApplicationRelation`). The baseline's
 *    own `apps/elitea-ui/src/hooks/application/useLibraryToolkits.js`'s
 *    `useAssociateToolkit` called this exact route with this exact body
 *    shape (`entity_type` hardcoded to `'agent'` regardless of
 *    agent-vs-pipeline — verified directly against that file, not a porting
 *    guess). `associateToolkitInstance` (below) calls it directly via
 *    `eliteaFetch`, the SAME primitive every generated hook calls
 *    internally — the established precedent for a route the generated
 *    client is missing (`features/toolkits/ui/ExportToolkitButton.tsx`'s own
 *    module doc comment sets this exact pattern for `export_toolkit/
 *    prompt_lib`). `onAttachToolkit`/`onAttachMcp` are no longer the attach
 *    MECHANISM — they are optional OBSERVER callbacks this component still
 *    fires after ITS OWN real attach call succeeds, for a caller that wants
 *    to know (e.g. a future GA-tracking or toast integration); selecting a
 *    Toolkit/MCP row always attaches it now, matching the baseline, with or
 *    without either callback supplied.
 *  - `ListToolkitInstancesParams` has `mcp` and `query` server-side filters
 *    (ordered by name, then id), as EliteaUI's picker uses. Each of the Toolkit
 *    and MCP dropdowns pages its OWN server-filtered cursor (`mcp=false` /
 *    `mcp=true`, search sent as `query`) — see `useToolkitInstancePager` and
 *    `InstanceAddSection` in `ToolMenuSections.tsx`.
 *  - `ListApplicationsParams` (agents/pipelines) has no page/pageSize
 *    param at all — the baseline's infinite-scroll agent/pipeline lists
 *    have no server-side "load more" to page through; this shows whatever
 *    the one available page returns, filtered by the search box (which DOES
 *    map onto the real `query` param).
 *  - `Application` (the generated agent/pipeline list-row type) has no
 *    `has_swarm`/`icon_meta` fields — the baseline's swarm-agent list
 *    filter and rich per-entry icon are both dropped; `Application.icon`
 *    (a plain URL string) is passed to `EntityIcon` instead.
 *  - Known integration gap: neither `updateApplicationRelation`'s response
 *    (agent/pipeline attach) nor `associateToolkitInstance`'s (toolkit/MCP
 *    attach, see the bullet above) carries a `tool_id`/constructed tool row
 *    — after ANY successful attach, this component invalidates its OWN
 *    `getGetApplicationQueryOptions` cache entry (so its internal "already
 *    added" / "unsaved" state stays correct) and calls `onToolsChanged`, but
 *    has no way to push the new tool into a SEPARATE `tools` list a parent
 *    component (e.g. the real `ApplicationTools`) owns — that parent must
 *    refetch its own copy on `onToolsChanged` too.
 *
 * **"Create new toolkit" round trip — inbound half (baseline's
 * `handleAddNewlyCreatedToolkit`/its `newToolkitId` watcher-effect,
 * `ToolMenu.jsx:172-230`).** `ToolMenuSections.tsx`'s `InstanceAddSection`
 * owns the OUTBOUND half (navigating to toolkit/MCP creation with
 * `SearchParams.ReturnUrl`/`SearchParams.SourceApplicationId` — see that
 * file's own module doc comment); this component owns the return watch.
 * `useSearch({ strict: false })` (this component isn't bound to one
 * specific route — `ApplicationTools.tsx` mounts it on both the agent and
 * pipeline editors) reads a returned `?newToolkitId=` id, fetches that toolkit
 * by id (`getToolkit`, so a toolkit that sorts past the first listing page is
 * still found) and — if it resolves — calls `attachToolkit`/`attachMcp`
 * (chosen by the fetched row's own `isMcpToolkit`, not the URL's `mcp` flag;
 * the same real-attach path a manual dropdown click uses,
 * `onAttachToolkit`/`onAttachMcp` fired only as their post-success observer)
 * before clearing all four round-trip params from the URL. A non-numeric id,
 * a 404 or any fetch error attaches nothing and still clears the params (once;
 * a failed fetch is not retried), matching baseline's "clean up even on
 * failure" behaviour with no toast, since none is threaded through this
 * component. One real, disclosed limit on this, not invented around:
 *  - The RETURN navigation itself (a not-yet-built toolkit-creation page
 *    reading `SearchParams.ReturnUrl`/`SourceApplicationId` and appending
 *    `?newToolkitId=`/`?mcp=` on save) is out of this unit's ownership
 *    fence — this effect only reacts to those params if and when some
 *    future page sets them; it is independently correct and testable
 *    regardless of who sets them.
 */
export interface ToolMenuProps {
  readonly applicationId?: number | string | undefined;
  /** Called after any successful attach (agent/pipeline/toolkit/MCP) — see the module doc comment's "known integration gap" paragraph. */
  readonly onToolsChanged?: (() => void) | undefined;
  /** Optional observer, fired after this component's OWN real toolkit-attach call succeeds — see the module doc comment's "REAL GAPS" bullet. Not required for attaching to work. */
  readonly onAttachToolkit?: ((toolkit: Toolkit) => void) | undefined;
  /** Same as `onAttachToolkit`, for the MCP dropdown. */
  readonly onAttachMcp?: ((toolkit: Toolkit) => void) | undefined;
}

const containerSx: SxProps<Theme> = { display: 'flex', gap: 1, alignItems: 'center', maxWidth: '100%', flexWrap: 'wrap' };
/** Not in `SearchParams` (`@/shared/lib/params`) — the baseline never put it in `constants.js` either (`ToolMenu.jsx:217`'s `searchParams.get('newToolkitId')` is a raw literal), only `src/routes/-search/params.ts`'s `paramSchemas.newToolkitId` registers it. */
const NEW_TOOLKIT_ID_PARAM = 'newToolkitId';

function toApplicationTool(tool: unknown): AgentToolAssociation {
  return tool as AgentToolAssociation;
}

function computeAddedToolkitIds(currentTools: readonly AgentToolAssociation[]): ReadonlySet<string | number> {
  const ids = currentTools
    .filter((tool) => tool.type !== 'application')
    .map((tool) => tool.id)
    .filter((id): id is string | number => id !== undefined);
  return new Set(ids);
}

function resolveEntityLabel(agentType: string | undefined): 'agent' | 'pipeline' {
  return agentType === 'pipeline' ? 'pipeline' : 'agent';
}

/**
 * `PATCH /elitea_core/tool/prompt_lib/{project_id}/{toolkit_id}` — attaches a toolkit instance
 * as a tool on the given version. See this module's own doc comment ("REAL GAPS" bullet) for
 * the full evidence trail (real Go route, no orval wrapper, baseline's exact body shape).
 * Resolves `true` on success, `false` on any failure — this file's other association path
 * (`associateAsAgent`/`associateAsPipeline`, via `useAgentPipelineAssociation`) also swallows
 * failures without surfacing an error (no toast infrastructure exists in this app yet, per that
 * hook's own doc comment); this mirrors the same silent-on-failure shape rather than inventing
 * error UI this file doesn't otherwise have.
 */
async function associateToolkitInstance(projectId: string, applicationId: number, versionId: number, toolkitId: string | number): Promise<boolean> {
  try {
    await setToolkitRelation({ projectId, applicationId, versionId, toolkitId, hasRelation: true });
    return true;
  } catch {
    return false;
  }
}

/* ── the application/version this ToolMenu is attached to ─────────────────── */

interface ToolMenuEntityDetail {
  readonly numericApplicationId: number | undefined;
  readonly versionId: number | undefined;
  readonly isEntityUnsaved: boolean;
  readonly entityLabel: 'agent' | 'pipeline';
  readonly currentTools: readonly AgentToolAssociation[];
  readonly addedToolkitIds: ReadonlySet<string | number>;
}

function useToolMenuEntityDetail(applicationId: number | string | undefined, projectId: string | undefined): ToolMenuEntityDetail {
  const numericApplicationId = applicationId === undefined ? undefined : Number(applicationId);
  const applicationQuery = useGetApplication(projectId ?? '', numericApplicationId ?? 0, {
    query: { enabled: projectId !== undefined && numericApplicationId !== undefined },
  });
  // Same error-envelope-unreachable cast as every other Wave-2 generated-client caller (eliteaFetch throws instead of resolving with the error variant).
  const versionDetails = (applicationQuery.data as { data: ApplicationDetail } | undefined)?.data.version_details;
  const versionId = versionDetails === undefined || versionDetails.id === undefined ? undefined : Number(versionDetails.id);
  const currentTools = useMemo(() => (versionDetails?.tools ?? []).map(toApplicationTool), [versionDetails?.tools]);
  const addedToolkitIds = useMemo(() => computeAddedToolkitIds(currentTools), [currentTools]);

  return {
    numericApplicationId,
    versionId,
    isEntityUnsaved: numericApplicationId === undefined || versionId === undefined,
    entityLabel: resolveEntityLabel(versionDetails?.agent_type),
    currentTools,
    addedToolkitIds,
  };
}

export function ToolMenu({ applicationId, onToolsChanged, onAttachToolkit, onAttachMcp }: ToolMenuProps): ReactNode {
  const projectId = useSelectedProjectId();
  const queryClient = useQueryClient();
  const isMcpVisible = useIsMcpVisible();

  const { numericApplicationId, versionId, isEntityUnsaved, entityLabel, currentTools, addedToolkitIds } = useToolMenuEntityDetail(applicationId, projectId);
  const { filterAgents, filterPipelines } = useFilterAddedItems(currentTools);

  // `getToolIcon` (agent/pipeline flat icon, faithfully ported) is also returned by this hook
  // for callers that render a tool CARD icon elsewhere (e.g. sibling A1h's `ToolCard`) — not
  // used by this component's own markup, which uses `EntityIcon`'s gradient-badge treatment
  // for menu ROWS instead, matching the baseline's own two-different-icon-treatments split.
  const { associateAgent } = useAgentPipelineAssociation({ projectId, applicationId: numericApplicationId, versionId });

  const onAssociated = useCallback(async () => {
    if (projectId !== undefined && numericApplicationId !== undefined) {
      await queryClient.invalidateQueries({ queryKey: getGetApplicationQueryKey(projectId, numericApplicationId) });
    }
    onToolsChanged?.();
  }, [projectId, numericApplicationId, queryClient, onToolsChanged]);

  const associateAsAgent = useCallback(
    (candidate: AssociationCandidate) => {
      void associateAgent(candidate, { isPipeline: false, currentTools }).then((outcome) => {
        if (outcome.ok) void onAssociated();
      });
    },
    [associateAgent, currentTools, onAssociated],
  );
  const associateAsPipeline = useCallback(
    (candidate: AssociationCandidate) => {
      void associateAgent(candidate, { isPipeline: true, currentTools }).then((outcome) => {
        if (outcome.ok) void onAssociated();
      });
    },
    [associateAgent, currentTools, onAssociated],
  );

  // Performs the REAL toolkit/MCP attach (see this module's own doc comment, "REAL GAPS"
  // bullet) — `notify` is the caller-supplied `onAttachToolkit`/`onAttachMcp` observer, fired
  // only after the attach actually succeeds, never instead of it.
  const attachToolkitInstance = useCallback(
    (toolkit: Toolkit, notify: ((toolkit: Toolkit) => void) | undefined) => {
      if (projectId === undefined || numericApplicationId === undefined || versionId === undefined) return;
      void associateToolkitInstance(projectId, numericApplicationId, versionId, toolkit.id).then((ok) => {
        if (!ok) return;
        void onAssociated();
        notify?.(toolkit);
      });
    },
    [projectId, numericApplicationId, versionId, onAssociated],
  );
  const attachToolkit = useCallback((toolkit: Toolkit) => attachToolkitInstance(toolkit, onAttachToolkit), [attachToolkitInstance, onAttachToolkit]);
  const attachMcp = useCallback((toolkit: Toolkit) => attachToolkitInstance(toolkit, onAttachMcp), [attachToolkitInstance, onAttachMcp]);

  // "Create new toolkit" round trip — inbound half. See this module's own doc comment.
  const navigate = useNavigate();
  const returnSearch = useSearch({ strict: false }) as Readonly<Record<string, unknown>>;
  const processedReturnedToolkitIds = useRef<Set<string>>(new Set());
  useEffect(() => {
    const rawNewToolkitId = returnSearch[NEW_TOOLKIT_ID_PARAM];
    // A route without a `text()` schema hands a numeric id over as a number (the search parser is JSON-based).
    const newToolkitId = typeof rawNewToolkitId === 'string' || typeof rawNewToolkitId === 'number' ? String(rawNewToolkitId) : undefined;
    if (newToolkitId === undefined || projectId === undefined || isEntityUnsaved) return;
    if (processedReturnedToolkitIds.current.has(newToolkitId)) return;
    processedReturnedToolkitIds.current.add(newToolkitId);

    const toolId = Number(newToolkitId);
    const resolved = Number.isInteger(toolId) && toolId > 0 ? getToolkit(projectId, toolId).then((response) => unwrapBody(response) as Toolkit) : Promise.resolve(undefined);
    void resolved
      .catch(() => undefined)
      .then((toolkit) => {
        if (toolkit !== undefined) (isMcpToolkit(toolkit) ? attachMcp : attachToolkit)(toolkit);
        void navigate({
          to: '.',
          search: (prev: Record<string, unknown>) => {
            const next = { ...prev };
            delete next[NEW_TOOLKIT_ID_PARAM];
            delete next[SearchParams.IsMCP];
            delete next[SearchParams.SourceApplicationId];
            delete next[SearchParams.ReturnUrl];
            return next;
          },
          replace: true,
        });
      });
  }, [returnSearch, projectId, isEntityUnsaved, attachToolkit, attachMcp, navigate]);

  const [agentAnchor, setAgentAnchor] = useState<HTMLElement | null>(null);
  const [agentSearch, setAgentSearch] = useState('');
  const [pipelineAnchor, setPipelineAnchor] = useState<HTMLElement | null>(null);
  const [pipelineSearch, setPipelineSearch] = useState('');
  const closeAgentMenu = useCallback(() => {
    setAgentAnchor(null);
    setAgentSearch('');
  }, []);
  const closePipelineMenu = useCallback(() => {
    setPipelineAnchor(null);
    setPipelineSearch('');
  }, []);

  const agentsQuery = useListApplications(projectId ?? '', { agents_type: 'classic', query: agentSearch }, { query: { enabled: projectId !== undefined } });
  const pipelinesQuery = useListApplications(projectId ?? '', { agents_type: 'pipeline', query: pipelineSearch }, { query: { enabled: projectId !== undefined } });
  // Memoised because `unwrapList` returns a FRESH array and both feed
  // `useEntityAssociationItems`'s dep list — the old `?? EMPTY_APPLICATIONS`
  // constant only stabilised the empty case, not the populated one.
  const agentRows = useMemo(() => unwrapList<Application>(agentsQuery.data, 'listApplications(agents)'), [agentsQuery.data]);
  const pipelineRows = useMemo(() => unwrapList<Application>(pipelinesQuery.data, 'listApplications(pipelines)'), [pipelinesQuery.data]);

  const agentItems = useEntityAssociationItems(agentRows, numericApplicationId, filterAgents, 'agent', associateAsAgent);
  const pipelineItems = useEntityAssociationItems(pipelineRows, numericApplicationId, filterPipelines, 'pipeline', associateAsPipeline);

  // One distinct tooltip per button (baseline: `ToolMenu.jsx:564-650`, four separate `Tooltip`
  // titles) — NOT one shared string reused across all four, which would blur "add toolkits" vs
  // "add mcps" vs "add agents" vs "add pipelines" into the same generic copy.
  const saveFirstToolkitTooltip = t('agents.toolMenu.saveFirstToolkit', 'Save the {{entity}} first, then add toolkits', { entity: entityLabel });
  const saveFirstMcpTooltip = t('agents.toolMenu.saveFirstMcp', 'Save the {{entity}} first, then add mcps', { entity: entityLabel });
  const saveFirstAgentTooltip = t('agents.toolMenu.saveFirstAgent', 'Save the {{entity}} first, then add agents', { entity: entityLabel });
  const saveFirstPipelineTooltip = t('agents.toolMenu.saveFirstPipeline', 'Save the {{entity}} first, then add pipelines', { entity: entityLabel });

  return (
    <Box sx={containerSx}>
      <InstanceAddSection
        copy={{ label: t('agents.toolMenu.toolkit', 'Toolkit'), searchPlaceholder: t('agents.toolMenu.searchToolkits', 'Search toolkits...'), emptyMessage: t('agents.toolMenu.noToolkits', 'No toolkits available') }}
        testId="agent-add-toolkit-button"
        isEntityUnsaved={isEntityUnsaved}
        tooltip={saveFirstToolkitTooltip}
        isMcp={false}
        projectId={projectId}
        addedToolkitIds={addedToolkitIds}
        onAttach={attachToolkit}
        createRoute="/toolkits/create"
        sourceApplicationId={numericApplicationId}
      />

      {isMcpVisible && (
        <InstanceAddSection
          copy={{ label: t('agents.toolMenu.mcp', 'MCP'), searchPlaceholder: t('agents.toolMenu.searchMcps', 'Search mcps...'), emptyMessage: t('agents.toolMenu.noMcps', 'No mcps available') }}
          // Symmetric with `agent-add-toolkit-button` above. Without it the
          // only handle on this control is its LABEL, which is translated —
          // `e2e/streaming/chat.mcp.spec.ts` drives it, and a locale change
          // must not be able to move the handle out from under the journey.
          testId="agent-add-mcp-button"
          isEntityUnsaved={isEntityUnsaved}
          tooltip={saveFirstMcpTooltip}
          isMcp={true}
          projectId={projectId}
          addedToolkitIds={addedToolkitIds}
          onAttach={attachMcp}
          createRoute="/mcps/create"
          sourceApplicationId={numericApplicationId}
        />
      )}

      <EntityAddSection
        copy={{ label: t('agents.toolMenu.agent', 'Agent'), searchPlaceholder: t('agents.toolMenu.searchAgents', 'Search agents...'), emptyMessage: t('agents.toolMenu.noAgents', 'No agents available') }}
        isEntityUnsaved={isEntityUnsaved}
        tooltip={saveFirstAgentTooltip}
        items={agentItems}
        isFetching={agentsQuery.isFetching}
        search={agentSearch}
        onSearchChange={setAgentSearch}
        onOpen={setAgentAnchor}
        anchor={agentAnchor}
        onClose={closeAgentMenu}
      />

      <EntityAddSection
        copy={{ label: t('agents.toolMenu.pipeline', 'Pipeline'), searchPlaceholder: t('agents.toolMenu.searchPipelines', 'Search pipelines...'), emptyMessage: t('agents.toolMenu.noPipelines', 'No pipelines available') }}
        isEntityUnsaved={isEntityUnsaved}
        tooltip={saveFirstPipelineTooltip}
        items={pipelineItems}
        isFetching={pipelinesQuery.isFetching}
        search={pipelineSearch}
        onSearchChange={setPipelineSearch}
        onOpen={setPipelineAnchor}
        anchor={pipelineAnchor}
        onClose={closePipelineMenu}
      />
    </Box>
  );
}
