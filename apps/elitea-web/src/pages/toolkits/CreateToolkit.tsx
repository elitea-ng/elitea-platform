import { type ReactNode, useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { useNavigate, useParams } from '@tanstack/react-router';

import { CreateToolkitToolTabBar, ToolkitForm, ToolkitTypeSelector, type ToolkitEditorDeps, toolkitEditorHooks, useToolkitCreate } from '@/features/toolkits';
import { t } from '@/shared/i18n';

import { useToolkitCredentialPickerSlot } from './lib/credentialPickerSlots';
import { SHAREPOINT_AUTH_MODALS } from './lib/sharepointAuthModals';
import { useSelectedProjectId } from './lib/useSelectedProjectId';
import { useMcpDiscoverySlot } from './lib/useMcpDiscoverySlot';
import type { EditToolDetail } from './lib/toolkitFormTypes';

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
const errorSx: SxProps<Theme> = { marginBottom: '1rem' };

export interface CreateToolkitDeps {
  /** Same `UseToolkitCreateMutation` shape as `useToolkitCreate()` (`features/toolkits`' `api/toolkits.ts`, the real generated `POST /elitea_core/tools/prompt_lib/{projectId}`) — injected only by tests or callers wrapping the write. */
  readonly createToolkit: ToolkitEditorDeps['createToolkit'];
}

export interface CreateToolkitProps {
  readonly isMCP?: boolean;
  readonly isApplication?: boolean;
  /**
   * OPTIONAL since Phase 1c. `createToolkit` is now a real generated
   * operation, so this page supplies `useToolkitCreate()` itself and callers
   * only pass `deps` to override it (tests, or a caller that needs to wrap
   * the write). It used to be required because no generated endpoint
   * existed — see the CORRECTION in `features/toolkits/api/toolkits.ts`.
   */
  readonly deps?: CreateToolkitDeps;
}

interface CreateToolkitRouteParams {
  readonly toolkitType?: string;
}

/** `ToolkitForm`'s `onSave` is a required prop, but this page always renders it with `hideOperationButtons` — see the module doc comment. */
function noopSave(): Promise<Readonly<Record<string, unknown>>> {
  return Promise.resolve({});
}

/** `toolkit_name`-flagged settings-key resolution — local duplicate of `SaveToolkitButton.tsx`'s `toolkitNameSettingsKey` (this same slice, not exported — its own budget is already at the §3.5 ceiling, see `features/toolkits/index.ts`'s own doc comment). */
function toolkitNameFromSettings(editToolDetail: EditToolDetail): string | undefined {
  const schema = editToolDetail.schema as { readonly properties?: Readonly<Record<string, { readonly toolkit_name?: boolean }>> } | undefined;
  const key = Object.keys(schema?.properties ?? {}).find((candidate) => schema?.properties?.[candidate]?.toolkit_name);
  return key === undefined ? undefined : (editToolDetail.settings?.[key] as string | undefined);
}

/**
 * Local duplicate of `CreateToolkitToolTabBar.tsx`'s `isPrebuildMcpType`
 * (`features/toolkits`, this same predicate) — not imported because that
 * symbol is not on the slice's public API and `features/toolkits/index.ts`
 * sits at its 20/20 §3.5 `slice-public-api` ceiling (its own budget-note
 * comment); deep imports are barred by `no-deep-slice-import`. Pre-built MCP
 * toolkit types are prefixed `mcp_`, while the bare `'mcp'` type means
 * "remote MCP", not pre-built.
 */
function isPrebuildMcpType(toolkitType: string | undefined): boolean {
  return typeof toolkitType === 'string' && toolkitType.startsWith('mcp_') && toolkitType !== 'mcp';
}

/**
 * Which detail route a successful create lands on — the baseline's
 * destination branch (`apps/elitea-ui/src/pages/Toolkits/
 * CreateToolkitToolTabBar.jsx:148-156`): pre-built `mcp_*` types go to the
 * MCP detail page even when created from `/toolkits/create`.
 */
export function createdEntityKind(isMCP: boolean, isApplication: boolean, toolkitType: string | undefined): 'mcp' | 'app' | 'toolkit' {
  if (isMCP || isPrebuildMcpType(toolkitType)) return 'mcp';
  return isApplication ? 'app' : 'toolkit';
}

function resolveCreateTitle(isApplication: boolean, isMCP: boolean): string {
  if (isApplication) return t('pages.toolkits.createToolkit.titleApplication', 'New Application');
  return isMCP ? t('pages.toolkits.createToolkit.titleMcp', 'New MCP') : t('pages.toolkits.createToolkit.titleToolkit', 'New Toolkit');
}

/**
 * Ported from `apps/elitea-ui/src/pages/Toolkits/CreateToolkit.jsx` (274
 * lines) — ROUTE-027/028 `/toolkits/create[/:toolkitType]` (+ the
 * `/mcps/create[/:toolkitType]` sibling, `isMCP`; `/apps/create[/:appType]`
 * also renders this same baseline component with `isApplication` — see
 * below). Unit A4g.
 *
 * DISCLOSED DEVIATIONS:
 *  - **No ambient Formik context.** `editToolDetail`/`formValues` are
 *    explicit local state, matching every sibling `features/*`/`pages/*`
 *    port's "no Formik" convention this whole batch already established
 *    (`CreateApplication.tsx`'s own doc comment).
 *  - **The create mutation is real** (`POST /elitea_core/tools/prompt_lib/
 *    {projectId}` — `useToolkitCreate`, see `features/toolkits`' `api/
 *    toolkits.ts` Phase-1c CORRECTION), supplied by this page itself;
 *    `deps.createToolkit` remains only as a test/wrapper override. All three
 *    live routes (`src/routes/_shell/{toolkits,mcps,apps}/create.tsx`)
 *    render this page. On success the page REPLACES the create route with
 *    the created entity's detail route (`createdEntityKind` above — the
 *    baseline's `CreateToolkitToolTabBar.jsx:148-166` destination branch;
 *    replace so Back does not reopen a stale, still-dirty create form).
 *    NOT YET PORTED from that same baseline block: the `ReturnUrl`/
 *    `SourceApplicationId` round-trip (returning to a source agent/pipeline
 *    with `newToolkitId` for auto-association) — no create route in this
 *    app declares those search params yet, so there is nothing to read.
 *  - **The baseline's `CreateToolkitToolTabBar` save button used to fire a
 *    global `eventEmitter` event another component listened for.** The
 *    ALREADY-PORTED `CreateToolkitToolTabBar.tsx` (this same unit) dropped
 *    that indirection — its own doc comment: "The caller... owns: The
 *    actual save trigger and its network call." This page IS that caller —
 *    `handleSave` below builds the create-mutation body directly from
 *    `editToolDetail`/`formValues` and calls `deps.createToolkit`.
 *  - **`ToolkitForm`'s `formValues`/`formInitialValues`/`onSetFormField`
 *    track name/description separately from `editToolDetail`** (that
 *    component's own `editField`: name/description edits route through
 *    `onSetFormField`, everything else through `onChangeToolDetail` — see
 *    that file's own source). This page wires `onSetFormField` to update
 *    `formValues` state, unlike the ALREADY-LANDED `ToolkitEditor.tsx`'s
 *    create-mode body (`ToolkitEditorParts.tsx`'s `ToolkitEditorBody`),
 *    which does not pass `onSetFormField` at all — noted as an
 *    already-existing, out-of-this-unit's-current-change gap in that file,
 *    not repeated here since wiring it is a one-line addition.
 *  - **Custom-`create_url` iframe fallback DROPPED, real backend gap, not a
 *    porting shortcut.** The baseline's `toolSchema?.metadata?.interface?.
 *    create_url` (+ the sibling `EditToolkit.jsx`'s `app_url`) is the SAME
 *    class of "let this specific integration serve its own custom UI"
 *    mechanism `pages/apps/AppDetail.tsx`'s own `useAppDetail.ts` already
 *    found has NO backend support: `grep -rn "create_url\|app_url"
 *    --include="*.go" services/` (elitea-main) returns zero hits — the
 *    field is never populated by the Go backend this app talks to (same
 *    dead-legacy-plugin-runtime class as `custom_ui_route`/`ui_host`, see
 *    that hook's own doc comment for the full citation). The standard
 *    form/selector flow below is therefore the ONLY reachable path, not a
 *    narrowed one.
 *  - **GA event tracking** — dropped outright, same documented gap every
 *    other editor in this session gives (no analytics-event SDK exists).
 *  - **`isApplication` is accepted (and the title/copy branches on it) for
 *    baseline call-site parity** (the SAME component renders `/apps/create`
 *    in the baseline — `ProtectedRoutes.jsx:229-230`), but wiring the real
 *    `/apps/create` route to THIS page (vs. a separate `pages/apps`-owned
 *    page) is a decision for whoever owns route-to-page wiring / the `apps`
 *    domain (unit A6, already landed in batch 1) — not this unit's call to
 *    make unilaterally. Flagged, not silently re-architected.
 */
export function CreateToolkit({ isMCP = false, isApplication = false, deps }: CreateToolkitProps): ReactNode {
  const defaultCreateToolkit = useToolkitCreate();
  const createToolkitMutation = deps?.createToolkit ?? defaultCreateToolkit;
  const navigate = useNavigate();
  const params = useParams({ strict: false }) as CreateToolkitRouteParams;
  const projectId = useSelectedProjectId();

  const [editToolDetail, setEditToolDetail] = useState<EditToolDetail | null>(null);
  const [formValues, setFormValues] = useState<Readonly<Record<string, unknown>>>({ type: params.toolkitType ?? '' });
  const [isDirty, setIsDirty] = useState(false);
  const [isCreating, setIsCreating] = useState(false);
  const [createError, setCreateError] = useState<unknown>(undefined);
  /**
   * #613. The server now refuses a save whose credential reference does not
   * resolve, and answers with per-field `settings_errors`. Without this
   * supplier the page swallowed that body whole: the catch below turned every
   * 400 into the one fixed banner string, and `ToolkitForm`'s server-error
   * channel had no producer anywhere in the app.
   */
  const saveValidation = toolkitEditorHooks.useToolkitSaveValidation();

  /*
   * #308 — the credential picker, supplied here for the same reason
   * `EditToolkit.tsx` supplies it: `features/toolkits` may not import
   * `features/credentials` (`no-sideways-features`), and this page may import
   * both. A toolkit being created needs a credential before its first save,
   * so this root cannot be skipped.
   *
   * `sharepointAuthModals` used to sit in a module-level constant. It moved
   * into this `useMemo` because the picker is hook-derived (it closes over
   * `projectId`) and both slots must travel in one object.
   */
  const renderCredentialPicker = useToolkitCredentialPickerSlot(projectId);

  const handleSelectTool = useCallback((detail: EditToolDetail) => {
    setEditToolDetail(detail);
    setIsDirty(true);
  }, []);

  const handleSetFormInitialValues = useCallback((updater: (prev: Readonly<Record<string, unknown>>) => Readonly<Record<string, unknown>>) => {
    setFormValues(updater);
  }, []);

  const handleChangeToolDetail = useCallback(
    (updater: (prev: EditToolDetail | null) => EditToolDetail | null) => {
      setIsDirty(true);
      setEditToolDetail(updater);
      // The previous refusal described the PREVIOUS settings. Dropping it here
      // is what re-arms Save: `handleSave` below refuses to re-issue a request
      // whose recorded refusal is still on screen, so without this the gate
      // would stay latched shut on an error the user has already fixed.
      saveValidation.clearSaveErrors();
    },
    [saveValidation],
  );

  const handleSetFormField = useCallback((field: string, value: unknown) => {
    setFormValues((prev) => ({ ...prev, [field]: value }));
  }, []);
  const toolActionsExtra = useMcpDiscoverySlot({ editToolDetail, onChangeToolDetail: handleChangeToolDetail, projectId });
  const toolkitFormSlots = useMemo(
    () => ({ sharepointAuthModals: SHAREPOINT_AUTH_MODALS, renderCredentialPicker, toolActionsExtra }),
    [renderCredentialPicker, toolActionsExtra],
  );

  const handleClearEditTool = useCallback(() => {
    setEditToolDetail(null);
    setFormValues({ type: params.toolkitType ?? '' });
    setIsDirty(false);
  }, [params.toolkitType]);

  const handleSave = useCallback(async () => {
    if (projectId === undefined || editToolDetail?.type === undefined) return;
    // A refusal that is still on screen is not re-submitted: the body has not
    // changed, so the answer would not either. Any real edit drops it
    // (handleChangeToolDetail below), which is what re-arms Save.
    //
    // DISCLOSED, NOT FIXED HERE: this page still passes no
    // `onValidationStateChange`, so LOCAL field errors — a blank name on a type
    // whose schema requires one — do not gate Save the way they do on the other
    // two save paths (`SaveToolkitButton`/`CreateToolkitButton`). Wiring that
    // gate makes a nameless create impossible, which is a separate behaviour
    // change from this one and breaks three existing tests that deliberately
    // save without a name.
    if (saveValidation.toolkitValidation.isError) return;
    setIsCreating(true);
    setCreateError(undefined);
    saveValidation.clearSaveErrors();
    try {
      const name = toolkitNameFromSettings(editToolDetail) ?? (formValues['name'] as string | undefined);
      const description = formValues['description'] as string | undefined;
      const created = await createToolkitMutation({
        projectId,
        type: editToolDetail.type,
        ...(name !== undefined ? { name } : {}),
        ...(description !== undefined ? { description } : {}),
        settings: editToolDetail.settings,
        meta: editToolDetail.meta,
      });
      // A successful create used to be silently discarded here — the form
      // stayed dirty and a second Save duplicated the toolkit. Baseline
      // parity (`CreateToolkitToolTabBar.jsx:148-166`): REPLACE the create
      // route with the created entity's detail route, so Back does not
      // reopen a stale create form. See the module doc for the not-yet-
      // ported `ReturnUrl`/`SourceApplicationId` round-trip.
      setIsDirty(false);
      const kind = createdEntityKind(isMCP, isApplication, editToolDetail.type);
      const id = String(created.id);
      if (kind === 'mcp') {
        void navigate({ to: '/mcps/$tab/$mcpId', params: { tab: 'all', mcpId: id }, replace: true });
      } else if (kind === 'app') {
        void navigate({ to: '/apps/$tab/$appId', params: { tab: 'all', appId: id }, replace: true });
      } else {
        void navigate({ to: '/toolkits/$tab/$toolkitId', params: { tab: 'all', toolkitId: id }, replace: true });
      }
    } catch (error) {
      // A refusal that names fields is shown ON those fields; the generic
      // banner is kept for everything else (a 500, a dropped connection),
      // where there is no field to point at.
      setCreateError(saveValidation.reportSaveError(error) ? undefined : error);
    } finally {
      setIsCreating(false);
    }
  }, [projectId, editToolDetail, formValues, createToolkitMutation, navigate, isMCP, isApplication, saveValidation]);

  const title = useMemo(() => resolveCreateTitle(isApplication, isMCP), [isApplication, isMCP]);

  return (
    <Box sx={pageSx}>
      <Box sx={tabBarSx}>
        <Typography variant="headingSmall">{title}</Typography>
        {editToolDetail && (
          <CreateToolkitToolTabBar
            toolkitType={editToolDetail.type}
            isDirty={isDirty}
            isSaving={isCreating}
            isMCP={isMCP}
            isApplication={isApplication}
            onSave={() => {
              void handleSave();
            }}
            onClearEditTool={handleClearEditTool}
          />
        )}
      </Box>
      <Box sx={contentSx}>
        {createError !== undefined && (
          <Typography
            role="alert"
            variant="bodyMedium"
            sx={errorSx}
          >
            {t('pages.toolkits.createToolkit.error', 'Failed to create the toolkit.')}
          </Typography>
        )}
        {editToolDetail ? (
          <ToolkitForm
            editToolDetail={editToolDetail}
            onChangeToolDetail={handleChangeToolDetail}
            isEditing={false}
            isToolDirty={isDirty}
            hideConfigurationNameInput
            projectId={projectId}
            formValues={formValues}
            formInitialValues={formValues}
            onSetFormField={handleSetFormField}
            hideOperationButtons
            isMCP={isMCP}
            onSave={noopSave}
            toolkitValidation={saveValidation.toolkitValidation}
            slots={toolkitFormSlots}
          />
        ) : (
          <ToolkitTypeSelector
            onSelectTool={handleSelectTool}
            setFormikInitialValues={handleSetFormInitialValues}
            isMCP={isMCP}
            isApplication={isApplication}
          />
        )}
      </Box>
    </Box>
  );
}
