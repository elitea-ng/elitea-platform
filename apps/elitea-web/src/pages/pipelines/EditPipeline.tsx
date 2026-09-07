import { useCallback, useMemo, useState, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useParams, useSearch } from '@tanstack/react-router';
import { FormProvider } from 'react-hook-form';

import { ConfigurationTab, resetPipelineDraft, usePipelineVersionSync } from '@/features/pipelines';
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import { t } from '@/shared/i18n';
import { AgentModelSettings } from '@/widgets/agent-model-settings';
import { disarmUnsavedChangesNavBlocker, useUnsavedChangesNavBlocker } from '@/widgets/app-shell';

import { pipelineDetailDisplayName, toVersionSummaries } from './lib/editPipelineMappers';
import { isPublicPipelinesProject } from './lib/isPublicPipelinesProject';
import {
  buildPipelineConfigurationTabSlots,
  PipelineConfigurationTabBoundary,
} from './lib/pipelineConfigurationTabGaps';
import { useRefetchPipelineAfterSave } from './lib/useRefetchPipelineAfterSave';
import { usePipelineChatAdapter } from './lib/usePipelineChatAdapter';
import { usePipelineChatSlotContext } from './lib/usePipelineChatSlotContext';
import { usePipelineEditorUser } from './lib/usePipelineEditorUser';
import { useCorrectUserNameInUrl } from './lib/useCorrectUserNameInUrl';
import { useEditPipelineConfigurationTabBridge } from './lib/useEditPipelineConfigurationTabBridge';
import { useEditPipelineData } from './lib/useEditPipelineData';
import { useEditPipelineForm } from './lib/useEditPipelineForm';
import { useIsVersionNotFound } from './lib/useIsVersionNotFound';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { EditPipelineActions } from './ui/EditPipelineActions';
import { EditPipelineAlerts } from './ui/EditPipelineAlerts';
import { EditPipelineNotFound } from './ui/EditPipelineNotFound';
import { EditPipelineSaveBar } from './ui/EditPipelineSaveBar';
import { EditPipelineVersionBar } from './ui/EditPipelineVersionBar';

/**
 * The `||` lives here rather than inline in `EditPipeline` purely to keep
 * that component under the §3.5 oxlint cyclomatic-complexity budget (12) —
 * the same reason `./lib/useEditPipelineData.ts` splits half its body into
 * one-line helpers. Behaviour is exactly `useUnsavedChangesNavBlocker(
 * isFormDirty || isYamlDirty)`; see the call site for what each half means.
 * `isFormDirty` is `useEditPipelineForm`'s combined flag — the RHF fields AND
 * the model picker, which lives outside the form.
 */
function useEditPipelineNavBlocker(isFormDirty: boolean, isYamlDirty: boolean): void {
  useUnsavedChangesNavBlocker(isFormDirty || isYamlDirty);
}

/**
 * Whether the editor has nothing to show YET — as opposed to "a request is in
 * flight", which is what `isFetching` means and which is also true for a
 * background refetch.
 *
 * `ConfigurationTab` renders a spinner INSTEAD of the editor while its
 * `isFetching` is set, unmounting the whole canvas and both side panels. That
 * was harmless while nothing ever refetched; `useRefetchPipelineAfterSave`
 * made it happen after every save, and the cost was measured in a browser: the
 * editor blanked, both panels reopened, and their collapse states were gone.
 * TanStack keeps the previous `detail` while it revalidates, so this keeps the
 * first-load spinner and leaves a refetch invisible.
 *
 * A module-scope function rather than an inline `&&` for the same reason
 * `useEditPipelineNavBlocker` above is one: the page is at the §3.5
 * cyclomatic-complexity ceiling (12) and one more branch breaches it.
 */
function editorIsLoading(isFetching: boolean, detail: unknown): boolean {
  return isFetching && detail === undefined;
}

const pageSx: SxProps<Theme> = { height: '100%', display: 'flex', flexDirection: 'column' };
const tabBarSx: SxProps<Theme> = {
  flexShrink: 0,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  borderBottom: 1,
  borderColor: 'divider',
  padding: '0 1.5rem',
  minHeight: '3rem',
};
const contentSx: SxProps<Theme> = { flex: 1, minHeight: 0, overflowY: 'auto', padding: '1.5rem' };

interface EditPipelineParams {
  readonly tab?: string;
  readonly agentId?: string;
  readonly version?: string;
}

interface EditPipelineSearch {
  readonly isFromCreation?: string;
}

function parseApplicationId(agentId: string | undefined): number | undefined {
  if (agentId === undefined || !/^\d+$/.test(agentId)) return undefined;
  return Number(agentId);
}

/**
 * Ported from `apps/elitea-ui/src/pages/Pipelines/EditPipeline.jsx` —
 * ROUTE-020 `/pipelines/:tab/:agentId` (+ optional `/:version`,
 * ROUTE-069; spec §8.1). Structurally the pipelines-domain mirror of
 * `pages/agents/EditApplication.tsx` (Wave-2 unit A1g) — a Pipeline
 * literally IS an Application row, and this page fetches/saves through the
 * exact same `entities/application-form`/generated-client seam.
 *
 * The application-detail/version fetch (`useEditPipelineData`) and the RHF
 * form + imperative save (`useEditPipelineForm`) both live in `./lib/` —
 * split out purely to stay under the §3.5 400-line budget and the oxlint
 * cyclomatic-complexity budget (12).
 *
 * **Composition gaps, disclosed:**
 *  - The baseline's `ConfigurationTab` (`Components/ConfigurationTab.jsx`,
 *    `EditorPanel.jsx`, `FlowWrapper.jsx`, `GeneralFormPanel.jsx`,
 *    `ChatPanel.jsx`, `AddNodeMenu.jsx`) landed later, from a sibling A2
 *    sub-unit, as `features/pipelines/ui/ConfigurationTab.tsx` — exported
 *    from that slice's `index.ts` specifically so this page could reach it
 *    (adversarial-review fix: this page used to render an unconditional
 *    empty placeholder `<Box>` here even after `ConfigurationTab` existed,
 *    leaving the entire standalone pipeline editor blank). It is now
 *    mounted for real below, wrapped in `PipelineConfigurationTabBoundary`
 *    (`./lib/pipelineConfigurationTabGaps.tsx`). Two of the three sub-gaps
 *    that module used to disclose are now CLOSED, and their stated reasons
 *    were stale rather than merely optimistic — read its corrected header:
 *    the test-chat slot renders the real `widgets/chat-box` `ChatBox`
 *    (`./ui/PipelineTestChat.tsx`), the `adapter` is real
 *    (`./lib/usePipelineChatAdapter.ts`), and the boundary's "nobody mounts
 *    a `SocketClientContext.Provider`" claim was simply false —
 *    `app/providers/AppProviders.tsx` mounts one around every page. What
 *    remains a genuine gap is the configuration FORM's agent-domain panels.
 *  - `ApplicationTabBar`/`ApplicationControls`
 *    (`@/[fsd]/entities/application-tab-bar/ui`) were NOT promoted into any
 *    `entities/` slice (verified: no `entities/application-tab-bar`
 *    directory exists in this worktree). This page reuses the promoted
 *    `CreateApplicationTabBar` for save/discard instead, same substitution
 *    `pages/agents/EditApplication.tsx` already made — the baseline's real
 *    consumer of `useDiscardPipelineChanges`'s baseline counterpart
 *    (`useDiscardApplicationChanges`) is in fact `ApplicationTabBar`
 *    (grepped directly), so wiring it here is the closest faithful home
 *    available.
 *  - Nav-blocking-when-dirty (`useNavBlocker`, baseline) is CLOSED (#133).
 *    It was disclosed here as dropped for want of a promoted equivalent;
 *    in fact `widgets/app-shell`'s `NavBlockerDialog` held a real app-wide
 *    TanStack `useBlocker` all along and only lacked a setter outside the
 *    chat process. Armed below off `formState.isDirty || isYamlDirty` —
 *    see that call site for the one residual it discloses.
 *  - The flow GRAPH now round-trips (#135): `usePipelineVersionSync` seeds
 *    the editor stores from this version's `instructions` YAML + saved
 *    `pipeline_settings`, and `useEditPipelineForm` reads the live graph back
 *    out through `usePipelineGraphDraft` on save. `tags`/`tools` still have
 *    no field on this endpoint — see `entities/application-form/model/
 *    mutations.ts`'s own doc comment for what remains of that gap.
 *
 * **Read-only (public-viewer) gating, save-failure feedback, and the
 * detail-404 page** (adversarial-review fixes, reproduced verbatim from
 * `pages/agents/EditApplication.tsx`'s own equivalent fix, Wave-2 unit
 * A1g): the baseline's `useViewMode()` hides the Save/Save-New-Version
 * buttons whenever the currently selected project is the public project
 * (`ApplicationTabBar.jsx:65`); this page reproduces that default via the
 * same `isPublicPipelinesProject` check `Pipelines.tsx`/`usePipelinesData.ts`
 * (this unit) already use for the identical "is the current viewing context
 * public" question — no override is threaded through a `viewMode` search
 * param because nothing in this unit's own navigation call sites
 * (`Latest`/`MyLiked`/`Trending`/`PrivatePipelinesList`) ever sets one, so
 * reading it here would not change the actual, reachable behaviour. A
 * failed save now surfaces via `useEditPipelineForm`'s `saveError`, rendered
 * as an inline `role="alert"` banner (this app has no toast infrastructure
 * — see that hook's own doc comment). A 404/400 on the pipeline-DETAIL
 * fetch itself now renders the same dedicated not-found `NoResultsMessage`
 * this file already used for an unknown version id, instead of falling
 * through to the normal edit-page shell — old app: `EditPipeline.jsx`'s
 * `shouldShowNotFoundPage = (isError && isNotFoundError(error)) ||
 * isVersionNotFound` → `<Page404 />`.
 */
export function EditPipeline(): ReactNode {
  const projectId = useSelectedProjectId();
  const params = useParams({ strict: false }) as EditPipelineParams;
  const search = useSearch({ strict: false }) as EditPipelineSearch;
  const applicationId = parseApplicationId(params.agentId);
  const requestedVersionId = params.version;

  const { detail, versions, activeVersion, isFetching, isError, isDetailNotFound, explicitVersionId } = useEditPipelineData(
    projectId,
    applicationId,
    requestedVersionId,
  );

  const versionSummaries = useMemo(() => toVersionSummaries(versions), [versions]);
  const isVersionMissing = useIsVersionNotFound({
    version: requestedVersionId,
    isFetching,
    isError,
    versions: versionSummaries,
    skip: search.isFromCreation === 'true',
  });

  useCorrectUserNameInUrl(detail?.name);

  // #135 (read half): parse this version's `instructions` YAML and its saved
  // `pipeline_settings` geometry into the two flow-editor stores `ConfigurationTab`
  // -> `EditorPanel` render from. Without this the standalone editor page always
  // started from an empty document — a stored pipeline's graph was never shown,
  // so a save could only ever have written an empty graph back.
  usePipelineVersionSync({ isCreateMode: false, versionDetails: activeVersion, versionId: activeVersion?.id });

  const { form, handleSave, isSaving, saveError, llmSettings, isDirty, admissionRefused } = useEditPipelineForm(
    detail,
    activeVersion,
    projectId,
    applicationId,
  );
  const { setFieldValue, versionDetails } = useEditPipelineConfigurationTabBridge(activeVersion, form.setValue);
  useRefetchPipelineAfterSave(isSaving, saveError, projectId, applicationId, explicitVersionId);
  // The real `ChatConversationAdapter`, and the signed-in user the test chat's
  // conversation names as its author — both page-owned because `pages/` is the
  // layer allowed to reach `entities/conversation`, `entities/participant` and
  // `widgets/chat-box` at once. See `./lib/pipelineConfigurationTabGaps.tsx`.
  const chatAdapter = usePipelineChatAdapter();
  const chatUser = usePipelineEditorUser();
  /*
   * #133 — this used to be a write-only `const [, setIsYamlDirty]`: the
   * flow editor reported its dirtiness and the page dropped it. It is now
   * half of the page's answer to "does this editor hold unsaved work?".
   * (It is still NOT a save gate — the save path carries the live graph
   * since #135, so blocking save on unsaved YAML would be actively wrong:
   * unsaved YAML is exactly what Save is for. A visible dirty-state
   * indicator is still unbuilt.)
   */
  const [isYamlDirty, setIsYamlDirty] = useState(false);

  /*
   * #133 — arm the app-wide unsaved-changes guard. `widgets/app-shell`'s
   * `NavBlockerDialog` + its TanStack `useBlocker` are mounted under every
   * page and work; nothing on the standalone `/pipelines` editors ever
   * raised the flag, so a nav-link click discarded the edit silently. This
   * closes the "nav-blocking-when-dirty is dropped" gap this file's header
   * used to disclose.
   *
   * DISCLOSED, and deliberately in the OVER-prompting direction: a
   * successful save clears the RHF half (`useEditPipelineForm` resets the
   * form) but NOT `isYamlDirty`, because `useIsPipelineYamlCodeDirty`
   * compares the live `yamlCode` against `initYamlCode`, and the save path
   * invalidates no GET-side query (`useSaveApplicationVersion`'s own doc
   * comment) so nothing re-seeds that baseline. A user who edited the
   * canvas and saved therefore still gets one "unsaved changes?" prompt on
   * their next nav-away. That is the fail-safe direction — the same one
   * `NavBlockerDialog`'s header argues for — and strictly better than the
   * silent data loss it replaces. The real fix is re-seeding the editor
   * stores from the save's response; that needs a `features/pipelines`
   * export this page does not have.
   */
  useEditPipelineNavBlocker(isDirty, isYamlDirty);

  // Old app: `useViewMode.js` — `viewMode` defaults to `ViewMode.Public`
  // whenever the currently selected project equals `PUBLIC_PROJECT_ID`.
  // `ApplicationTabBar.jsx:65` only renders the Save/Save-New-Version
  // buttons when `viewMode !== ViewMode.Public` — every viewer reaching
  // this page through this unit's own public-project tabs (`Latest`/
  // `MyLiked`/`Trending`/the public "Admin" `PrivatePipelinesList`, none of
  // which pass a `viewMode` override on navigation) is a read-only viewer
  // of someone else's public pipeline, not its owner.
  const isReadOnlyView = isPublicPipelinesProject(projectId);
  const isEditorLoading = editorIsLoading(isFetching, detail);

  /*
   * Confirming the discard dialog now actually DROPS the draft and LEAVES
   * editing, mirroring `pages/agents/EditApplication.tsx`'s `handleDiscarded`.
   * Measured defect (the blocker in this page's twin, plus one worse half
   * here): Discard was `form.reset()` alone — but the save path reads the
   * LIVE graph (`usePipelineGraphDraft`) and the un-reset `llmSettings`, so
   * a later Save silently PERSISTED the discarded canvas/model edits, and
   * the user stayed on the edit page with no way out. The store resets run
   * BEFORE the navigation so the in-memory draft is gone even while this
   * page is still mounted; the disarm must precede the navigation or the
   * just-reset form's blocker prompts a second time.
   */
  const llmSettingsReset = llmSettings.reset;
  const navigate = useNavigate();
  const handleDiscarded = useCallback(() => {
    resetPipelineDraft();
    llmSettingsReset();
    disarmUnsavedChangesNavBlocker();
    void navigate({ to: '/pipelines/$tab', params: { tab: params.tab ?? 'latest' } });
  }, [llmSettingsReset, navigate, params.tab]);

  const setLlmSettings = llmSettings.setValue;
  const handleModelSettingsChange = useCallback((next: AgentLlmSettings) => setLlmSettings(next), [setLlmSettings]);
  // Everything the test-chat slot needs to name the pipeline it talks to.
  const chatSlotContext = usePipelineChatSlotContext({ projectId, applicationId: params.agentId, detail, activeVersion, user: chatUser });

  /*
   * The model picker rides in `ConfigurationTab`'s configuration-form slot —
   * the left panel, where the baseline puts model settings — rather than
   * above the editor, because that slot IS the configuration form and the
   * rest of it is still a disclosed gap (`./lib/pipelineConfigurationTabGaps
   * .tsx`). It is the only version-level field this page can edit today.
   */
  const configurationTabSlots = useMemo(
    () =>
      buildPipelineConfigurationTabSlots(
        <AgentModelSettings
          projectId={projectId}
          value={llmSettings.value}
          onChange={handleModelSettingsChange}
          disabled={isReadOnlyView || isFetching}
        />,
        chatSlotContext,
      ),
    [projectId, llmSettings.value, handleModelSettingsChange, isReadOnlyView, isFetching, chatSlotContext],
  );

  // Both dead ends render from `./ui/EditPipelineNotFound.tsx` — same copy,
  // same keys; see that file for why they left this one.
  if (isDetailNotFound) return <EditPipelineNotFound kind="pipeline" />;
  if (isVersionMissing) return <EditPipelineNotFound kind="version" />;

  return (
    <FormProvider {...form}>
      <Box sx={pageSx}>
        <Box sx={tabBarSx}>
          <Typography variant="headingSmall">
            {detail ? pipelineDetailDisplayName(detail) : t('pages.pipelines.editPipeline.title', 'Pipeline')}
          </Typography>
          {/*
            The version bar: version dropdown, Set-as-default, Delete version
            and Save-As-Version. Rendered OUTSIDE the writer-only block below
            on purpose — a public viewer keeps the selector and loses only the
            write affordances, which is what `AgentVersionControls`' own
            `canSaveNewVersion` gate (and `ApplicationTabBar.jsx:65`) means.
          */}
          <EditPipelineVersionBar
            projectId={projectId}
            applicationId={applicationId}
            tab={params.tab}
            versions={versions}
            activeVersion={activeVersion}
            isReadOnly={isReadOnlyView}
            /* `isEditorLoading`, NOT `isFetching` — the same distinction, and
               the same measured cost, that `editorIsLoading` above records for
               the canvas. `useRefetchPipelineAfterSave` refetches the detail
               after EVERY save, and while that request is in flight a raw
               `isFetching` unmounted this whole header row: the version
               selector, "Save As Version", Share/Fork and the Save bar all
               left the screen and came back. Journey J16b measured the
               consequence as a 30s wait for a "Save As Version" button that
               was not in the document — an author who saves and then takes a
               version sees the control they are reaching for disappear. */
            isFetching={isEditorLoading}
            llmSettings={llmSettings.value}
          />
          {!isEditorLoading && !isReadOnlyView && (
            <>
              {/* The Chat action — the only way to actually TALK to this
                  pipeline; see `./ui/ChatWithPipelineButton.tsx` for the
                  participant mapping and why it is writer-only. */}
              <EditPipelineActions
                applicationId={params.agentId}
                detail={detail}
                activeVersion={activeVersion}
                projectId={projectId}
                tab={params.tab}
              />
              <EditPipelineSaveBar
                onSave={handleSave}
                canSave={form.formState.isValid && !isSaving}
                isSaving={isSaving}
                onDiscarded={handleDiscarded}
              />
            </>
          )}
        </Box>
        <Box sx={contentSx}>
          <EditPipelineAlerts
            isError={isError}
            admissionRefused={admissionRefused}
            saveError={saveError}
          />
          <PipelineConfigurationTabBoundary>
            <ConfigurationTab
              isFetching={isEditorLoading}
              isError={isError}
              applicationId={applicationId}
              pipelineName={detail ? pipelineDetailDisplayName(detail) : undefined}
              versionDetails={versionDetails}
              versions={versions}
              setFieldValue={setFieldValue}
              setYamlDirty={setIsYamlDirty}
              adapter={chatAdapter}
              slots={configurationTabSlots}
            />
          </PipelineConfigurationTabBoundary>
        </Box>
      </Box>
    </FormProvider>
  );
}
