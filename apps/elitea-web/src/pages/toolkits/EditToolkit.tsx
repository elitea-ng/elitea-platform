import { type ComponentProps, type ReactNode, useCallback, useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useParams } from '@tanstack/react-router';

import { ConfigurationTab, DeleteToolkitButton, ExportToolkitButton, IndexesTab, ToolkitsControls, type ToolkitEditorDeps, useToolkitEdit } from '@/features/toolkits';
import type { ToolkitInstance } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { ViewMode } from '@/shared/lib/enums';
import { BaseTab } from '@/shared/ui/BaseTab';
import { BaseTabs } from '@/shared/ui/BaseTabs';

import { useScheduleCredentialsSelectSlot, useToolkitCredentialPickerSlot } from './lib/credentialPickerSlots';
import { useCopyLinkMenuItem, useToolkitActionPermissions } from './lib/useToolkitHeaderActions';
import { useToolkitSaveControls } from './lib/useToolkitSaveControls';
import { ToolkitSaveBar } from './ui/ToolkitSaveBar';
import { useConfigurationTabSlots } from './lib/configurationTabSlots';
import { useMcpLoadTools } from './lib/useMcpLoadTools';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { INDEXES_CHAT_UI } from './lib/indexesChatUI';
import type { EditToolDetail } from './lib/toolkitFormTypes';
import { useIndexesTabState } from './lib/useIndexesTabState';
import type { IndexesTabState } from './lib/useIndexesTabState';
import { useToolkitDetail } from './lib/useToolkitDetail';

const pageSx: SxProps<Theme> = { height: '100%', display: 'flex', flexDirection: 'column' };
const headerSx: SxProps<Theme> = {
  flexShrink: 0,
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  borderBottom: 1,
  borderColor: 'divider',
  padding: '0 1.5rem',
  minHeight: '3rem',
};
const actionsSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.5rem' };
const tabBarSx: SxProps<Theme> = { flexShrink: 0, borderBottom: 1, borderColor: 'divider', padding: '0 1.5rem' };
const contentSx: SxProps<Theme> = { flex: 1, minHeight: 0 };
/** The refused save's own words. Without it a rejected `PUT` left the screen looking exactly like a successful one. */
const saveErrorSx: SxProps<Theme> = { flexShrink: 0, padding: '0.5rem 1.5rem' };
const indexesPanelSx: SxProps<Theme> = { height: '100%', display: 'flex', minHeight: 0 };

export interface EditToolkitDeps {
  /** No generated `PUT /elitea_core/tool/prompt_lib/{projectId}/{toolId}` endpoint exists yet — see `features/toolkits`' `api/toolkits.ts` module doc comment. */
  readonly saveToolkit: ToolkitEditorDeps['saveToolkit'];
}

export interface EditToolkitProps {
  readonly isMCP?: boolean;
  /** OPTIONAL since Phase 1c — see `CreateToolkitProps.deps`. */
  readonly deps?: EditToolkitDeps;
}

interface EditToolkitRouteParams {
  readonly toolkitId?: string;
  readonly mcpId?: string;
  readonly appId?: string;
}

function toEditDetail(detail: ToolkitInstance | undefined): EditToolDetail | null {
  if (detail === undefined) return null;
  return { id: detail.id, type: detail.type, name: detail.name, description: detail.description, settings: detail.settings, meta: detail.meta };
}

/**
 * The Indexes tab panel. A separate component purely so `EditToolkit` stays
 * under the §3.5 complexity budget (12) — same reason
 * `useSaveToolkitMutation` below is not inlined. `toolkitId` is `undefined`
 * only while the route params are still resolving, at which point there is
 * no toolkit to list indexes for.
 */
interface IndexesTabPanelProps {
  readonly toolkitId: string | undefined;
  readonly state: IndexesTabState;
  readonly renderCredentialsSelect: ComponentProps<typeof IndexesTab>['renderCredentialsSelect'];
}

function IndexesTabPanel({ toolkitId, state, renderCredentialsSelect }: IndexesTabPanelProps): ReactNode {
  if (toolkitId === undefined) return null;
  return (
    <Box
      sx={indexesPanelSx}
      data-testid="edit-toolkit-indexes-tab-panel"
    >
      <IndexesTab
        toolkitId={toolkitId}
        values={state.toolkitValues}
        selectedIndexTools={state.selectedIndexTools}
        chatUI={INDEXES_CHAT_UI}
        renderCredentialsSelect={renderCredentialsSelect}
        // The worker-capability verdict off the served type schema. The KEY is
        // omitted when the worker can run this type (`exactOptionalPropertyTypes`),
        // and an empty string is a real verdict — see `IndexesTab`.
        {...(state.unavailableReason === undefined ? {} : { unavailableReason: state.unavailableReason })}
      />
    </Box>
  );
}

/** Split out of `EditToolkit` for the §3.5 complexity budget — see `IndexesTabPanel` above. */
function resolveTitle(isMCP: boolean, name: string | undefined): string {
  const fallback = isMCP ? t('pages.toolkits.editToolkit.titleMcp', 'Edit MCP') : t('pages.toolkits.editToolkit.title', 'Edit Toolkit');
  return name ?? fallback;
}

/**
 * Ported from `apps/elitea-ui/src/pages/Toolkits/EditToolkit.jsx` (476
 * lines) — ROUTE-030 `/toolkits/:tab/:toolkitId` (+ the `/mcps/:tab/:mcpId`
 * sibling, `isMCP`; also the old app's `AppDetail.jsx` fallback for
 * non-custom-UI applications — `pages/apps/AppDetail.tsx`'s own doc comment
 * discloses that composition gap on ITS side, not this file's). Unit A4g.
 *
 * DISCLOSED DEVIATIONS:
 *  - **No ambient Formik context / no `getValidateSchema` validation
 *    schema** — this page's editable state is `editToolDetail`
 *    (`useState`), synced from the real fetched detail; `ConfigurationTab`'s
 *    own embedded `ToolkitForm` owns field-level validation exactly as it
 *    does for every other caller in this unit.
 *  - **No generated GET-single-toolkit or PUT-edit-toolkit endpoint
 *    exists** — see `features/toolkits`' `api/toolkits.ts` module doc
 *    comment for the full, exhaustively-verified inventory.
 *    `./lib/useToolkitDetail.ts` derives the detail from the real
 *    `listToolkitInstances` collection client-side (same technique as that
 *    file's own `useToolkitDetail`, duplicated locally — see that hook's
 *    own doc comment for why); `deps.saveToolkit` is injected into
 *    `ConfigurationTab`'s `saveHandlers`, same "caller supplies the
 *    network call, this page owns 100% real orchestration" convention this
 *    whole batch already established.
 *  - **`LegacyOpenApiMigration.normalizeLegacyOpenApiToolkit` DROPPED.**
 *    That helper lives inside `features/toolkits/lib/helpers/
 *    legacyOpenApiMigration.helpers.ts`, not exported from that slice's
 *    public `index.ts` (whose budget is already at the §3.5 20-symbol
 *    ceiling — see `features/toolkits/index.ts`'s own doc comment), and the
 *    baseline's own comment on every call site marks it "TODO: DELETE after
 *    migration period (Q1 2026) — Legacy OpenAPI toolkit migration" — a
 *    self-described temporary shim, not core toolkit-editing behaviour.
 *    `toEditDetail` below maps the real `ToolkitInstance` row directly.
 *  - **The "Test" tab is DROPPED, not narrowed** — the baseline's own tab
 *    entry for it is `{ content: <></>, display: 'none' }` (`EditToolkit.jsx`,
 *    the tabs `useMemo`) — permanently hidden dead UI in the baseline
 *    itself, not a real surface this port removes.
 *  - **The "Indexes" tab is MOUNTED (issue #149), and gated.** It used to be
 *    a real, clickable tab label in front of `<Box data-testid="edit-toolkit-
 *    indexes-tab-panel" />` — an empty div — on the stated grounds that
 *    `IndexesContainer` was not on `features/toolkits`' public API. It now
 *    renders that slice through the single `IndexesTab` composition root
 *    (see that file's own module doc for why the eight intra-slice
 *    dependencies it binds are not spent on this slice's §3.5 budget), with
 *    `pages/`-supplied `chatUI` for the three components no `features/` file
 *    may legally import.
 *
 *    TWO defects surfaced by mounting it, both fixed here rather than
 *    worked around:
 *      * The baseline never offers this tab on an MCP screen
 *        (`EditToolkit.jsx:208`, `if (mcpId) return true` →
 *        `display: 'none'`), nor on a toolkit type whose schema carries no
 *        indexing tool (`:210-216`). This port had dropped that gate
 *        entirely. `useIndexesTabVisibility` restores it — which is why the
 *        JRNY-018 MCP journey now asserts the tab's ABSENCE.
 *      * The tab index could point at a panel that no longer exists once
 *        the gate can flip at runtime; `activeTab` collapses to
 *        Configuration instead of rendering an empty content area.
 *  - **`ToolkitsControls` IS now rendered** (regression fix, finding R3) —
 *    previously imported nowhere in this app, leaving it genuinely dead
 *    code. Export/Delete stay as the standalone `DeleteToolkitButton`/
 *    `ExportToolkitButton` icon buttons (same functional actions, a flat
 *    row instead of living inside the kebab — a disclosed layout
 *    simplification) with REAL `disabled` permission gating (finding R2):
 *    `./lib/useToolkitHeaderActions.ts`'s `useToolkitActionPermissions`
 *    reproduces the baseline `ToolkitsControls.
 *    jsx`'s own `checkPermission(PERMISSIONS.applications.{export,delete})
 *    && checkPermission(PERMISSIONS.toolkits.{export,delete})` gate via the
 *    real `usePermissionList` endpoint. `ToolkitsControls`'s own kebab now
 *    carries ONE real item, Copy Link (`./lib/useToolkitHeaderActions.ts`'s `useCopyLinkMenuItem`) — Pin,
 *    Fork, and the public-view Authors indicator remain genuinely
 *    unavailable: `usePin`/`useToolkitFork`-equivalent endpoints and an
 *    Authors/user-lookup UI have no port anywhere in this worktree (grepped:
 *    zero hits for either), and building either for real needs new files
 *    outside this cluster's owned scope (a `features/toolkits/api` pin/fork
 *    mutation wrapper, plus — for Fork — the baseline's unported
 *    `import-wizard` modal) — disclosed, not silently dropped.
 *  - **The Save/Cancel control IS now rendered** (`./ui/ToolkitSaveBar.tsx`,
 *    driven by `./lib/useToolkitSaveControls.ts`). It used to be missing
 *    outright: a saved toolkit could not be edited from its own page at all,
 *    because the only save path in the port —
 *    `ToolkitsOperationButtons.handleUpdateToolkit` — listens for
 *    `ToolEvents.ToolkitsUpdateToolkit` and nothing emitted it. The baseline
 *    emits it from its toolkit tab bar; this page is that emitter now, so the
 *    seam is wired rather than duplicated. Save is dirty-gated like the agent
 *    editor's, and REFUSED with the reason stated when the selected
 *    credential's stored check came back `auth_failed`/`unreachable` — see
 *    `./lib/useCredentialSaveGate.ts` for why the gate reads the probe's
 *    reason and never the `invalid` status.
 *  - **`redirect`/`iframe`-type application custom-UI branches, the
 *    embedding-model-change confirmation modal (`ToolkitsTabBar`'s own
 *    `isEmbeddingModelDirty` alert), the `destTab`/`name` URL-sync search
 *    params, and nav-blocking-while-dirty** are all dropped — same class of
 *    real, disclosed gaps `pages/agents/EditApplication.tsx`'s own doc
 *    comment gives for its structurally identical `useNavBlocker`/
 *    version-URL-sync omissions (no promoted equivalent exists; not this
 *    unit's owned scope to invent one).
 *  - **GA event tracking** — dropped outright, same documented gap every
 *    other editor in this session gives.
 */
/**
 * Resolve the save mutation OUTSIDE the component: inlining
 * `deps?.saveToolkit ?? useToolkitEdit()` there pushed `EditToolkit` past the
 * §3.5 complexity budget (12). The branching is the same, it just doesn't
 * count against the component.
 */
function useSaveToolkitMutation(deps: EditToolkitDeps | undefined): ReturnType<typeof useToolkitEdit> {
  const fallback = useToolkitEdit();
  return deps?.saveToolkit ?? fallback;
}

export function EditToolkit({ isMCP = false, deps }: EditToolkitProps): ReactNode {
  const saveToolkitMutation = useSaveToolkitMutation(deps);
  const params = useParams({ strict: false }) as EditToolkitRouteParams;
  const toolkitId = params.toolkitId ?? params.mcpId ?? params.appId;
  const projectId = useSelectedProjectId();

  const { detail, isFetching } = useToolkitDetail(projectId, toolkitId);
  const { canExport, canDelete } = useToolkitActionPermissions(projectId);
  const copyLinkMenuItems = useCopyLinkMenuItem();

  const [editToolDetail, setEditToolDetail] = useState<EditToolDetail | null>(null);
  const [isToolDirty, setIsToolDirty] = useState(false);
  const [tab, setTab] = useState(0);

  useEffect(() => {
    setEditToolDetail(toEditDetail(detail));
    setIsToolDirty(false);
  }, [detail]);

  const handleChangeToolDetail = useCallback((updater: (prev: EditToolDetail | null) => EditToolDetail | null) => {
    setIsToolDirty(true);
    setEditToolDetail(updater);
  }, []);

  const handleSaved = useCallback(() => setIsToolDirty(false), []);
  const handleDiscarded = useCallback(() => {
    setEditToolDetail(toEditDetail(detail));
    setIsToolDirty(false);
  }, [detail]);
  const saveControls = useToolkitSaveControls({ isDirty: isToolDirty, onSaved: handleSaved, onDiscarded: handleDiscarded });

  const handleTabChange = useCallback((_event: unknown, value: number) => setTab(value), []);

  /**
   * Issue #149. The baseline hides the Indexes tab outright on MCP screens
   * and on any toolkit whose type offers no indexing tool — this port had
   * dropped that gate and rendered a clickable tab in front of an empty Box
   * everywhere. See `features/toolkits`' `lib/helpers/indexesTabVisibility.ts`
   * for the baseline citation and the one disclosed schema-shape adaptation.
   */
  const indexesTab = useIndexesTabState({ isMCP, detail, editToolDetail, tab });

  // #308. This page is a legal composition root for BOTH `features/toolkits`
  // and `features/credentials`. Supplying these slots is what makes the toolkit
  // credential field, the schedule credential select and — for an MCP toolkit —
  // the "Load Tools" action render at all. See `./lib/useMcpLoadTools.tsx`.
  const renderCredentialPicker = useToolkitCredentialPickerSlot(projectId, saveControls.reportCredentialRefusal);
  const renderCredentialsSelect = useScheduleCredentialsSelectSlot(projectId);
  const mcpLoadTools = useMcpLoadTools({ projectId, editToolDetail, onChangeToolDetail: handleChangeToolDetail });
  const configurationTabSlots = useConfigurationTabSlots({ renderCredentialPicker, mcpLoadTools });

  const title = resolveTitle(isMCP, detail?.name);

  return (
    <Box sx={pageSx}>
      <Box sx={headerSx}>
        <Typography variant="headingSmall">{title}</Typography>
        {toolkitId !== undefined && (
          <Box sx={actionsSx}>
            <ExportToolkitButton
              toolkitId={toolkitId}
              name={detail?.name}
              disabled={!canExport}
            />
            <DeleteToolkitButton
              toolkitId={toolkitId}
              name={detail?.name}
              disabled={!canDelete}
            />
            <ToolkitsControls
              viewMode={ViewMode.Owner}
              menuItems={copyLinkMenuItems}
            />
            <ToolkitSaveBar
              onSave={saveControls.onSave}
              onDiscard={saveControls.onDiscard}
              canSave={saveControls.canSave}
              isSaving={saveControls.isSaving}
              disabledReason={saveControls.disabledReason}
            />
          </Box>
        )}
      </Box>
      <Box sx={tabBarSx}>
        <BaseTabs
          value={indexesTab.activeTab}
          onChange={handleTabChange}
          aria-label={title}
        >
          <BaseTab label={t('pages.toolkits.editToolkit.configurationTab', 'Configuration')} />
          {!indexesTab.hidden && <BaseTab label={t('pages.toolkits.editToolkit.indexesTab', 'Indexes')} />}
        </BaseTabs>
      </Box>
      {saveControls.saveError !== undefined && (
        <Typography
          role="alert"
          variant="bodyMedium"
          color="error"
          sx={saveErrorSx}
        >
          {saveControls.saveError}
        </Typography>
      )}
      <Box
        sx={contentSx}
        role="tabpanel"
      >
        {indexesTab.activeTab === 0 && (
          <ConfigurationTab
            isFetching={isFetching}
            applicationId={undefined}
            toolkitId={toolkitId}
            toolDetailState={{ editToolDetail, onChangeToolDetail: handleChangeToolDetail, isToolDirty }}
            isMCP={isMCP}
            projectId={projectId}
            onValidationStateChange={saveControls.onValidationStateChange}
            saveHandlers={{ saveToolkit: saveToolkitMutation, onSaveSuccess: saveControls.onSaveSuccess, onSaveError: saveControls.onSaveError }}
            slots={configurationTabSlots}
          />
        )}
        {indexesTab.activeTab === 1 && (
          <IndexesTabPanel
            toolkitId={toolkitId}
            state={indexesTab}
            renderCredentialsSelect={renderCredentialsSelect}
          />
        )}
      </Box>
    </Box>
  );
}
