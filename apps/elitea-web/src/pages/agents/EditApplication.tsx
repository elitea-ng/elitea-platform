import { useCallback, useMemo, type ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useParams, useSearch } from '@tanstack/react-router';
import { FormProvider } from 'react-hook-form';

import { AgentVersionControls } from '@/features/agents';
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import { t } from '@/shared/i18n';
import { NoResultsMessage } from '@/shared/ui/NoResultsMessage';
import { disarmUnsavedChangesNavBlocker, useUnsavedChangesNavBlocker } from '@/widgets/app-shell';

import { applicationDetailDisplayName, toVersionSummaries } from './lib/editApplicationMappers';
import { isPublicAgentsProject } from './lib/isPublicAgentsProject';
import { useCorrectUserNameInUrl } from './lib/useCorrectUserNameInUrl';
import { useEditApplicationData } from './lib/useEditApplicationData';
import { useEditApplicationEditorBridge } from './lib/useEditApplicationEditorBridge';
import { useEditApplicationForm } from './lib/useEditApplicationForm';
import { useEditApplicationVersionControls } from './lib/useEditApplicationVersionControls';
import { useEditApplicationVersionFields } from './lib/useEditApplicationVersionFields';
import { useIsVersionNotFound } from './lib/useIsVersionNotFound';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { EditApplicationActions } from './ui/EditApplicationActions';
import { EditApplicationAiEditSlot } from './ui/EditApplicationAiEditSlot';
import { EditApplicationConfigurationPanel } from './ui/EditApplicationConfigurationPanel';
import { EditApplicationEditorTabs } from './ui/EditApplicationEditorTabs';
import { EditApplicationSaveBar } from './ui/EditApplicationSaveBar';

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

interface EditApplicationParams {
  readonly tab?: string;
  readonly agentId?: string;
  readonly version?: string;
}

interface EditApplicationSearch {
  readonly isFromCreation?: string;
}

function parseApplicationId(agentId: string | undefined): number | undefined {
  if (agentId === undefined || !/^\d+$/.test(agentId)) return undefined;
  return Number(agentId);
}

/**
 * Ported from `apps/elitea-ui/src/pages/Applications/EditApplication.jsx`
 * — ROUTE-012 `/agents/:tab/:agentId` (+ optional `/:version`, ROUTE-067;
 * spec §8.1).
 *
 * The application-detail/version fetch (`useEditApplicationData`) and the
 * RHF form + imperative save (`useEditApplicationForm`) both live in
 * `./lib/` — split out of this file purely to stay under the §3.5 400-line
 * budget and the oxlint cyclomatic-complexity budget (12); see each hook's
 * own doc comment.
 *
 * **Composition gaps, disclosed:**
 *  - The baseline's `ConfigurationTab` (`Components/Applications/
 *    ConfigurationTab.jsx`) owns the six `ApplicationConfigurationLayout`
 *    slots (instructions, tools, welcome message, advance settings, editor
 *    notes, information) and still has no port. PARTIALLY CLOSED: the panel
 *    below no longer renders an empty `<Box/>`, and (#307) is no longer
 *    read-only — it renders
 *    `features/agents`' `CreateAgentForm`, the same component the baseline
 *    shares between its create and edit pages, so the agent's real
 *    name/description/instructions are on screen. The remaining slots
 *    (tools, advance settings, editor notes) are still absent.
 *  - `ApplicationTabBar`/`ApplicationControls`
 *    (`@/[fsd]/entities/application-tab-bar/ui`) were NOT promoted into any
 *    `entities/` slice (verified: no `entities/application-tab-bar`
 *    directory exists in this worktree) and are not claimed by name in this
 *    sub-unit's brief either. This page reuses the promoted
 *    `CreateApplicationTabBar` for save/discard instead — its own doc
 *    comment already documents that both the baseline's create- and
 *    edit-mode save buttons "collapse into the same presentational shape"
 *    once Formik/Redux orchestration is stripped; the real consumer of
 *    THIS unit's own `useDiscardApplicationChanges` in the baseline is in
 *    fact `ApplicationTabBar` (grepped directly — not `EditApplication.jsx`
 *    itself, which builds its OWN inline `handleDiscard`), so wiring it
 *    here is the closest faithful home available for an owned hook that
 *    would otherwise have no real call site in this unit.
 *  - Nav-blocking-when-dirty (`useNavBlocker`, baseline) is CLOSED (#133).
 *    It used to be dropped here on the grounds that no promoted equivalent
 *    existed; in fact `widgets/app-shell`'s `NavBlockerDialog` had a real,
 *    app-wide TanStack `useBlocker` all along and only lacked a setter
 *    outside the chat process, so a dirty agent edit was silently lost on
 *    any nav-link click. This page now arms it through
 *    `useUnsavedChangesNavBlocker` (see the call site below).
 *  - Saving is CLOSED (#307, and #345 for tags). It routes every field it
 *    renders — see the save-scope comment on the `useEditApplicationForm`
 *    call below for the one remaining backend-side gap
 *    (`conversation_starters` still has no input mounted).
 *
 * **Read-only (public-viewer) gating, save-failure feedback, and the
 * detail-404 page** (adversarial-review fixes): the baseline's
 * `useViewMode()` hides the Save/Save-New-Version buttons whenever the
 * currently selected project is the public project (`ApplicationTabBar.jsx:65`);
 * this page reproduces that default via the same `isPublicAgentsProject`
 * check `Applications.tsx`/`useApplicationsData.ts` (this unit) already use.
 * No override is threaded through the `viewMode` search param declared on
 * `/agents/$tab`: nothing in this unit's own navigation call sites
 * (`Latest`/`MyLiked`/`Trending`/`PrivateAgentsList`) ever sets it, so
 * reading it here would not change the reachable behaviour. A failed save
 * surfaces via `useEditApplicationForm`'s `saveError`, rendered as an
 * inline `role="alert"` banner (this app has no toast infrastructure). A
 * 404/400 on the application-DETAIL fetch renders the same dedicated
 * not-found `NoResultsMessage` this file already used for an unknown
 * version id — old app: `EditApplication.jsx`'s `shouldShowNotFoundPage =
 * (isError && isNotFoundError(error)) || isVersionNotFound` → `<Page404 />`.
 */
export function EditApplication(): ReactNode {
  const projectId = useSelectedProjectId();
  const params = useParams({ strict: false }) as EditApplicationParams;
  const search = useSearch({ strict: false }) as EditApplicationSearch;
  const applicationId = parseApplicationId(params.agentId);
  const requestedVersionId = params.version;

  const { detail, versions, activeVersion, isFetching, isError, isDetailNotFound } = useEditApplicationData(
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

  /*
   * #307 (+ tags, #345) — the version-level fields `CreateAgentForm` renders
   * live here rather than in the RHF form: `applicationCreationSchema` does
   * not validate them (same split `CreateApplication.tsx` makes for the
   * create page). Resolved BEFORE the form hook, which reads them.
   */
  const versionFields = useEditApplicationVersionFields(activeVersion);

  const { form, handleSave, isSaving, saveError, isDirty } = useEditApplicationForm(
    detail,
    activeVersion,
    projectId,
    applicationId,
    versionFields,
  );

  /*
   * #133 — arm the app-wide unsaved-changes guard from this page's own dirty
   * state. `widgets/app-shell`'s `NavBlockerDialog` and its TanStack
   * `useBlocker` are mounted under every page and have always worked; the
   * standalone agent editor simply never raised the flag, so editing an
   * agent and clicking any nav link navigated through and lost the edit.
   * (This replaces the "nav-blocking-when-dirty is dropped" gap this file's
   * header used to disclose.) `formState.isDirty` covers exactly the fields
   * this page routes back into the form — `name`/`description`, per
   * `useEditApplicationEditorBridge`'s `shouldDirty: true`. The hook
   * disarms on unmount; `useEditApplicationForm` clears dirtiness after a
   * successful save so saving is not itself prompted about.
   *
   * #307 — `versionFields.isDirty` is the other half: instructions/welcome
   * message/variables/step limit are held outside the form (see below), so
   * `formState.isDirty` cannot see them and a user who edited only those
   * would have navigated away without a prompt. Same two-part dirty check
   * `CreateApplication.tsx` already builds from its own `extraFields`.
   */
  useUnsavedChangesNavBlocker(isDirty);

  // Old app: `useViewMode.js` — `viewMode` defaults to `ViewMode.Public`
  // whenever the currently selected project equals `PUBLIC_PROJECT_ID`.
  // `ApplicationTabBar.jsx:65` only renders the Save/Save-New-Version
  // buttons when `viewMode !== ViewMode.Public` — every viewer reaching
  // this page through this unit's own public-project tabs (`Latest`/
  // `MyLiked`/`Trending`/the public "Admin" `PrivateAgentsList`, none of
  // which pass a `viewMode` override on navigation) is a read-only viewer
  // of someone else's public agent, not its owner. (Hoisted above the
  // version bar in #134: `AgentVersionControls` gates its own
  // "Save As Version" on the same flag, so it must be resolved first.)
  const isReadOnlyView = isPublicAgentsProject(projectId);

  /*
   * #134 — the version bar. `versionSummaries` above used to be this page's
   * ONLY use of the fetched version list, and it spent them on a 404 check:
   * versions were loaded and never shown. Both halves of the control the
   * baseline puts here (`ApplicationTabBar.jsx:58-68`) were already ported
   * and unreachable — `AgentPipelineVersionSelector` only from a tool card,
   * `SaveNewVersionButton` from nowhere at all.
   *
   * Route/clone/cache decisions all live in the hook — see its own doc
   * comment for why a version switch is a NAVIGATION here rather than the
   * baseline's in-place cache rewrite.
   */
  const versionControls = useEditApplicationVersionControls({
    projectId,
    applicationId,
    tab: params.tab,
    versions,
    activeVersion,
    control: form.control,
    versionFields: versionFields.fields,
    isReadOnly: isReadOnlyView,
    isFetching,
  });

  /*
   * The configuration panel used to be an empty `<Box data-testid=… />`, so
   * opening ANY agent showed a page with no fields on it. It renders
   * `features/agents`' `CreateAgentForm` — the same component the baseline
   * shares between its create and edit pages, and the one
   * `pages/agents/CreateApplication.tsx` already renders.
   *
   * SAVE SCOPE (#307, closed; #345 added `tags`): the panel is writable as
   * well as readable. `useEditApplicationForm` sends `name`/`description`
   * through `editApplication` and the version-level `instructions`/
   * `welcome_message`/`variables`/`tags`/`llm_settings`/`meta.step_limit`/
   * `conversation_starters` through `updateApplicationVersion` — the two
   * real endpoints `features/agents`' `useSaveVersion` already issues.
   *
   * ONE GAP REMAINS, backend-side and not closable from here:
   * `conversation_starters` has no input mounted (see the
   * `conversationStartersSlot` note on `CreateAgentForm`), so the value
   * round-trips from the server but cannot be changed. The `variables` gap
   * this used to record is closed (#307 wrote the UPDATE branch) and so is
   * the `tags` one (#345 added the wire field and the association write).
   */
  const editor = useEditApplicationEditorBridge(form, versionFields);
  // One expression, read twice below — the picker is disabled on exactly the
  // same terms as the rest of the form. Named rather than repeated because a
  // second inline `||` puts this component over the §3.5 complexity gate (12).
  const isEditorDisabled = isReadOnlyView || isFetching;

  // Routed through the same `onFieldChange` every other version-level field
  // uses, so the picked model lands in `useEditApplicationVersionFields` —
  // which is both what the save body reads and what `isDirty` above compares,
  // so picking a model and navigating away is prompted about (#133).
  const handleModelSettingsChange = useCallback(
    (next: AgentLlmSettings) => editor.onFieldChange('version_details.llm_settings', next),
    [editor],
  );

  // Confirming the discard dialog now LEAVES editing, matching the create
  // page's Cancel — measured defect: Discard reverted the fields and left the
  // user on the edit page with no way out. The disarm must precede the
  // navigation or the just-reset form's blocker prompts a second time.
  const navigate = useNavigate();
  const handleDiscarded = useCallback(() => {
    disarmUnsavedChangesNavBlocker();
    void navigate({ to: '/agents/$tab', params: { tab: params.tab ?? 'latest' } });
  }, [navigate, params.tab]);

  if (isDetailNotFound) {
    return (
      <Box sx={pageSx}>
        <NoResultsMessage
          title={t('pages.agents.editApplication.agentNotFound.title', 'Agent not found')}
          description={t('pages.agents.editApplication.agentNotFound.description', 'This agent no longer exists.')}
        />
      </Box>
    );
  }

  if (isVersionMissing) {
    return (
      <Box sx={pageSx}>
        <NoResultsMessage
          title={t('pages.agents.editApplication.notFound.title', 'Version not found')}
          description={t('pages.agents.editApplication.notFound.description', 'This version no longer exists.')}
        />
      </Box>
    );
  }

  return (
    <FormProvider {...form}>
      <Box sx={pageSx}>
        <Box sx={tabBarSx}>
          <Typography variant="headingSmall">
            {detail ? applicationDetailDisplayName(detail) : t('pages.agents.editApplication.title', 'Agent')}
          </Typography>
          {versionControls.showVersionControls && (
            <AgentVersionControls
              applicationId={versionControls.applicationIdText}
              projectId={projectId}
              versions={versionControls.versionOptions}
              activeVersionId={versionControls.activeVersionId}
              onSelectVersion={versionControls.handleSelectVersion}
              versionBody={versionControls.versionBody}
              canSaveNewVersion={versionControls.canSaveNewVersion}
              versionDelete={versionControls.versionDelete}
              onNewVersionSaved={versionControls.handleNewVersionSaved}
            />
          )}
          {/*
           * #307 — the entity-level actions (export, delete). Both were
           * ported, tested, and imported by nothing at all; see
           * `./ui/EditApplicationActions.tsx` for the baseline placement and
           * for why they are writer-only.
           */}
          {!isFetching && !isReadOnlyView && (
            <>
              <EditApplicationActions
                applicationId={params.agentId}
                detail={detail}
                activeVersion={activeVersion}
                tab={params.tab}
                projectId={projectId}
              />
              <EditApplicationSaveBar
                onSave={handleSave}
                canSave={form.formState.isValid && !isSaving}
                isSaving={isSaving}
                onDiscarded={handleDiscarded}
              />
            </>
          )}
        </Box>
        <Box sx={contentSx}>
          {isError && (
            <Typography
              role="alert"
              variant="bodyMedium"
            >
              {t('pages.agents.editApplication.error', 'Failed to load this agent.')}
            </Typography>
          )}
          {saveError !== undefined && (
            <Typography
              role="alert"
              variant="bodyMedium"
            >
              {t('pages.agents.editApplication.saveError', 'Failed to save your changes.')}
            </Typography>
          )}
          {/*
           * The Evaluation tab — the dimension library — mounted BESIDE the
           * configuration panel, which is where the baseline puts it
           * (`EditApplication.jsx:103-113`). It gets no route of its own:
           * an agent's evaluation criteria are a property of the agent being
           * edited, and reaching them should not mean leaving the editor.
           * See `./ui/EditApplicationEditorTabs.tsx` for why only the Library
           * sub-view exists.
           *
           * The configuration panel moved to its own file in the same change,
           * unaltered — two panels do not fit under the §3.5 400-line budget.
           */}
          <EditApplicationEditorTabs
            projectId={projectId}
            applicationId={applicationId}
            /*
             * The version an evaluation run scores. It comes from the version
             * controls, which is the same value the Save button writes to —
             * resolving it a second time inside the evaluation feature could
             * disagree with what the editor thinks is open.
             */
            applicationVersionId={versionControls.activeVersionId}
            configurationPanel={
              <EditApplicationConfigurationPanel
                projectId={projectId}
                applicationId={applicationId}
                activeVersion={activeVersion}
                editor={editor}
                versionFields={versionFields}
                isEditorDisabled={isEditorDisabled}
                isDirty={isDirty}
                isReadOnly={isReadOnlyView}
                onModelSettingsChange={handleModelSettingsChange}
                instructionsAiEditSlot={
                  <EditApplicationAiEditSlot editor={editor} projectId={projectId} disabled={isEditorDisabled} />
                }
              />
            }
          />
        </Box>
      </Box>
    </FormProvider>
  );
}
