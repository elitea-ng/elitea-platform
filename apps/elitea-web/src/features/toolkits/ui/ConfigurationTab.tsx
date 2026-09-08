import { type ReactNode, useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Grid from '@mui/material/Grid';
import type { SxProps, Theme } from '@mui/material/styles';

import { ViewRunHistoryButton } from '@/shared/ui/ViewRunHistoryButton';

import type { UseToolkitEditMutation } from '../api/toolkits';
import { useToolkitSaveValidation } from '../model/useToolkitSaveValidation';
import type { SharepointAuthModalRenderers } from '../sharepoint/ui/SharepointOAuthStatus';
import type { ToolBaseSlots } from './form/ToolBase/ToolBase.types';
import { ToolkitForm, type ToolkitFormEditDetail } from './form/ToolkitForm/ToolkitForm';
import { TestToolPane } from './test-tools/TestToolPane';
import type { SaveToolkitPayload } from './form/ToolkitForm/ToolkitsOperationButtons';

export interface ToolkitTestPaneRenderProps {
  readonly applicationId: string | undefined;
  readonly toolkitId: string | undefined;
  readonly isFullScreenChat: boolean;
  readonly onFullScreenChatChange: (value: boolean) => void;
  readonly onShowHistory?: () => void;
}

export interface ToolkitRunHistoryRenderProps {
  readonly toolkitId: string | undefined;
  readonly onClose: () => void;
}

/**
 * The left panel's editable-toolkit state — grouped into one prop, same
 * "group into one option object" §3.5 12-prop-budget move
 * `features/agents/ui/ConfigurationTab.tsx`'s own `testPaneSettings`
 * grouping and `InputBase.tsx`/`BasicAccordion.tsx` already document for
 * this codebase (this file's own prop list was 16 top-level props before
 * this grouping — over budget).
 */
export interface ToolkitDetailState {
  readonly editToolDetail: ToolkitFormEditDetail | null;
  /**
   * The optional second `options` argument mirrors `ToolkitForm`'s own
   * `onChangeToolDetail` contract (`ToolkitForm.types.ts`): `{ isAutoSelect:
   * true }` is fired when a child selector auto-picks a fallback value on
   * the user's behalf (e.g. a shared credential/embedding-model default) and
   * must NOT flip `isToolDirty` — baseline: `pages/Toolkits/
   * ConfigurationTab.jsx`'s own `onChangeToolDetail` (`if
   * (!options?.isAutoSelect) setIsToolDirty(...)`). This type only declares
   * the contract; the actual state owner (this prop's real caller, `pages/
   * toolkits/EditToolkit.tsx` — outside this slice's own files) is
   * responsible for honouring it, same as `ToolkitForm`'s own `editField`
   * forwards `options` through unchanged rather than swallowing it.
   */
  readonly onChangeToolDetail: (updater: (prev: ToolkitFormEditDetail | null) => ToolkitFormEditDetail | null, options?: { readonly isAutoSelect?: boolean }) => void;
  readonly isToolDirty?: boolean;
}

/** The save-mutation trio, grouped for the same §3.5 reason as {@link ToolkitDetailState}. */
export interface ToolkitSaveHandlers {
  /** No generated `PUT /elitea_core/tool/prompt_lib/{projectId}/{toolId}` endpoint exists yet — see `../api/toolkits.ts`'s module doc comment. */
  readonly saveToolkit: UseToolkitEditMutation;
  readonly onSaveSuccess?: (savedValues: Readonly<Record<string, unknown>>) => void;
  readonly onSaveError?: (message: string) => void;
}

/**
 * The three caller-supplied pieces this component cannot import itself,
 * grouped for the same §3.5 12-prop-budget reason as {@link ToolkitDetailState}
 * (adding `sharepointAuth` as a 13th top-level prop is what forced the
 * grouping).
 */
export interface ConfigurationTabSlots {
  /**
   * The RIGHT panel's live test-chat content — see the module doc comment for
   * why this is a slot, not a direct import.
   *
   * OPTIONAL, and normally omitted. Omitted, this component renders
   * `../ui/test-tools/TestToolPane.tsx`: the "pick a tool, fill its arguments,
   * Run, read the result" pane, which needs no `features/chat` and no
   * `widgets/` model selector and is therefore an ordinary intra-slice import.
   * Before that pane existed the only supplier of this slot was
   * `pages/toolkits/lib/configurationTabSlots.tsx`, and what it supplied was an
   * empty `<Box>` — a disclosed composition gap that made the whole right-hand
   * half of the toolkit editor blank.
   *
   * Supply it to render the FULLER surface (`./test-tools/TestTools.tsx`: a
   * live chat transcript and a model) the day this app has the two slices that
   * component needs.
   */
  readonly renderTestPane?: (props: ToolkitTestPaneRenderProps) => ReactNode;
  readonly renderRunHistory?: (props: ToolkitRunHistoryRenderProps) => ReactNode;
  /**
   * The `features/mcps` login/logout modals a SharePoint toolkit's delegated
   * (OAuth) login needs. `features/toolkits` cannot import `features/mcps`
   * (`no-sideways-features`), so the real `<McpAuthModal>`/`<McpLogoutModal>`
   * come from this component's `pages/`-layer caller — see
   * `pages/toolkits/lib/sharepointAuthModals.tsx`. Omitted, a SharePoint
   * toolkit's Login button runs its connection check and can open nothing,
   * which is exactly the dead state the whole path was in before it was
   * wired.
   */
  readonly sharepointAuth?: SharepointAuthModalRenderers;
  /**
   * #308 — the credential picker for a `configuration`-kind settings field.
   *
   * `features/toolkits` cannot import `features/credentials`
   * (`no-sideways-features`), so the real `<CredentialsSelect>` comes from
   * this component's `pages/`-layer caller — see
   * `pages/toolkits/lib/credentialPicker.tsx`. Same seam and same reason as
   * `sharepointAuth` above.
   *
   * Omitted, every `configuration` field renders as blank space with nothing
   * to click, which is the defect #308 records. The three MODEL pickers do not
   * depend on this slot: `useCredentialLikeFieldSlot` renders those itself.
   */
  readonly renderCredentialPicker?: ToolBaseSlots['renderCredentialPicker'];
  /**
   * The "Load Tools" action of an MCP toolkit's tool section.
   *
   * Same seam and same reason as the two above: `ToolActionsSelector` renders
   * the action from caller-injected props because the baseline's MCP fetch hook
   * lives in `features/mcps`. The `pages/`-layer caller supplies it — see
   * `pages/toolkits/lib/useMcpLoadTools.tsx`.
   *
   * Omitted, the action still renders and is permanently disabled, which is the
   * state every MCP toolkit's form was in before this slot was forwarded.
   */
  readonly toolActionsExtra?: ToolBaseSlots['toolActionsExtra'];
  readonly mcpAuthStatus?: ToolBaseSlots['mcpAuthStatus'];
}

/** @public */
export interface ConfigurationTabProps {
  readonly isFetching: boolean;
  readonly applicationId: string | undefined;
  readonly toolkitId: string | undefined;
  readonly toolDetailState: ToolkitDetailState;
  readonly hasNotSavedCredentials?: boolean;
  readonly updateKey?: string | number;
  readonly isMCP?: boolean;
  readonly projectId: string | undefined;
  readonly onValidationStateChange?: (state: { readonly hasErrors: boolean; readonly triggerValidation: () => void }) => void;
  readonly saveHandlers: ToolkitSaveHandlers;
  readonly slots: ConfigurationTabSlots;
}

/**
 * Ported from `apps/elitea-ui/src/pages/Toolkits/ConfigurationTab.jsx`.
 *
 * DISCLOSED REDESIGN — slot-based right panel, matching the precedent
 * `features/agents/ui/ConfigurationTab.tsx` already established for the
 * IDENTICAL architectural problem on the agents side (that file's own doc
 * comment, point 1): the baseline's right pane renders `TestTools`
 * (`features/toolkits/ui`, a sibling A4 sub-unit's own file — see the
 * mission preamble's "ToolkitsList" gap for the same "not owned by A4g, not
 * yet landed" class of dependency) and `RunHistoryContainer`
 * (`entities/run-history`, which does not exist anywhere in this worktree).
 * `renderTestPane`/`renderRunHistory` defer both to whichever future
 * page-level composition has the real sibling components in scope, exactly
 * as `renderTestPane`/`renderRunHistory` already do on the agents side.
 *
 * The LEFT panel's real content (`ToolkitForm`, `../ui/form/ToolkitForm/
 * ToolkitForm.tsx`) IS a real, landed, intra-slice import (unlike agents'
 * still-missing `ApplicationConfigurationForm`) — wired directly, not
 * slotted. `saveToolkit` is injected into it (no generated PUT endpoint —
 * see `../api/toolkits.ts`'s module doc comment); this component adapts the
 * `ToolkitFormEditDetail`/`id`/`projectId` triad into `ToolkitForm`'s own
 * `SaveToolkitPayload` shape.
 *
 * Dropped, disclosed:
 *  - `DirtyDetector`/`setDirty` (Formik-based) — this app has no Formik;
 *    dirty tracking is `ToolkitForm`'s own `isToolDirty` prop, caller-owned,
 *    same "not this component's concern" precedent `features/agents/ui/
 *    ConfigurationTab.tsx`'s own doc comment gives.
 *  - `useShowRunHistoryFromUrl` (URL-search-param auto-open) — router/page
 *    orchestration; `showHistory` is local state here, toggled via
 *    `onShowHistory`/`renderRunHistory`'s own `onClose`.
 */
/**
 * The toolkit AS EDITED, not as saved.
 *
 * `settings.selected_tools` is the explicit tool list the test pane's picker
 * reads first, so a tool the user has just ticked is offered without a save
 * first — which is the whole point of a test pane on an editing screen. The
 * keys are conditionally spread rather than set to `undefined`:
 * `exactOptionalPropertyTypes` makes "absent" and "present and undefined" two
 * different types, and `ToolkitConversationValues` declares the first.
 */
function toTestPaneValues(detail: ToolkitFormEditDetail | null): { readonly type?: string; readonly settings?: Readonly<Record<string, unknown>> } {
  return {
    ...(detail?.type === undefined ? {} : { type: detail.type }),
    ...(detail?.settings === undefined ? {} : { settings: detail.settings }),
  };
}

/**
 * The right-hand pane: the caller's own, or this slice's `TestToolPane`.
 *
 * Its own component rather than a ternary inside `ConfigurationTab`, which sits
 * at the §3.5 complexity budget (12): the branch plus the two conditional prop
 * keys pushed it to 14.
 */
interface TestPaneAreaProps {
  readonly render: ConfigurationTabSlots['renderTestPane'];
  readonly projectId: string | undefined;
  readonly toolkitId: string | undefined;
  readonly editToolDetail: ToolkitFormEditDetail | null;
  readonly applicationId: string | undefined;
  readonly isFullScreenChat: boolean;
  readonly onFullScreenChatChange: (value: boolean) => void;
  readonly onShowHistory: () => void;
}

function TestPaneArea(props: TestPaneAreaProps): ReactNode {
  const { render, projectId, toolkitId, editToolDetail, applicationId, isFullScreenChat, onFullScreenChatChange, onShowHistory } = props;
  if (render !== undefined) {
    return render({
      applicationId,
      toolkitId,
      isFullScreenChat,
      onFullScreenChatChange,
      ...(toolkitId !== undefined ? { onShowHistory } : {}),
    });
  }
  return (
    <TestToolPane
      projectId={projectId}
      toolkitId={toolkitId}
      values={toTestPaneValues(editToolDetail)}
    />
  );
}

export function ConfigurationTab({
  isFetching,
  applicationId,
  toolkitId,
  toolDetailState,
  hasNotSavedCredentials = false,
  updateKey,
  isMCP,
  projectId,
  onValidationStateChange,
  saveHandlers,
  slots,
}: ConfigurationTabProps): ReactNode {
  const { renderTestPane, renderRunHistory, sharepointAuth, renderCredentialPicker, toolActionsExtra, mcpAuthStatus } = slots;
  const { editToolDetail, onChangeToolDetail, isToolDirty } = toolDetailState;
  const { saveToolkit, onSaveSuccess, onSaveError } = saveHandlers;
  /**
   * #613 — the server's per-field save refusal, owned here rather than by the
   * page, because `handleSave` below is the only thing on this screen that
   * issues the write. `ToolkitForm`'s server-error channel had no producer
   * anywhere in the app before this.
   */
  const saveValidation = useToolkitSaveValidation();
  const [showHistory, setShowHistory] = useState(false);
  const [isFullScreenChat, setIsFullScreenChat] = useState(false);

  // Memoized so `ToolBase`'s `slots` identity is stable across renders (the
  // prop bag it lives in is rebuilt every render by design — see
  // `ToolkitForm.hooks.ts`'s own note — but a fresh object here would also
  // remount nothing, it would just churn; keep it cheap and stable).
  const formSlots = useMemo<ToolBaseSlots | undefined>(() => {
    if (sharepointAuth === undefined && renderCredentialPicker === undefined && toolActionsExtra === undefined && mcpAuthStatus === undefined) return undefined;
    return {
      ...(sharepointAuth === undefined ? {} : { sharepointAuthModals: sharepointAuth }),
      ...(renderCredentialPicker === undefined ? {} : { renderCredentialPicker }),
      ...(toolActionsExtra === undefined ? {} : { toolActionsExtra }),
      ...(mcpAuthStatus === undefined ? {} : { mcpAuthStatus }),
    };
  }, [sharepointAuth, renderCredentialPicker, toolActionsExtra, mcpAuthStatus]);

  const handleShowHistory = useCallback(() => setShowHistory(true), []);
  const handleCloseHistory = useCallback(() => setShowHistory(false), []);

  const handleSave = useCallback(
    async (payload: SaveToolkitPayload): Promise<Readonly<Record<string, unknown>>> => {
      if (payload.projectId === undefined || payload.toolId === undefined) return {};
      saveValidation.clearSaveErrors();
      try {
        const result = await saveToolkit({
          projectId: payload.projectId,
          toolId: String(payload.toolId),
          type: (payload.values['type'] as string | undefined) ?? '',
          ...(payload.name !== undefined ? { name: payload.name } : {}),
          description: payload.values['description'] as string | undefined,
          settings: payload.values['settings'] as Readonly<Record<string, unknown>> | undefined,
          meta: payload.values['meta'] as Readonly<Record<string, unknown>> | undefined,
        });
        return { ...result };
      } catch (error) {
        // Recorded, then RETHROWN: `ToolkitForm`'s own caller still owns the
        // `onSaveError` message and the failed-save state. Swallowing here
        // would make a refused save look like a successful one.
        saveValidation.reportSaveError(error);
        throw error;
      }
    },
    [saveToolkit, saveValidation],
  );

  if (isFetching) {
    return (
      <Box sx={spinnerContainerSx}>
        <CircularProgress />
      </Box>
    );
  }

  if (showHistory && renderRunHistory) {
    return <>{renderRunHistory({ toolkitId, onClose: handleCloseHistory })}</>;
  }

  return (
    <Grid
      sx={gridContainerSx}
      columnSpacing="2rem"
      container
    >
      {editToolDetail && !isFullScreenChat && (
        <Grid
          size={{ xs: 12, lg: 4 }}
          sx={leftGridItemSx}
        >
          <ToolkitForm
            editToolDetail={editToolDetail}
            onChangeToolDetail={onChangeToolDetail}
            isEditing
            isToolDirty={isToolDirty}
            showNameFieldForcedly
            showToolkitIcon
            hideConfigurationNameInput
            hasNotSavedCredentials={hasNotSavedCredentials}
            updateKey={updateKey}
            isMCP={isMCP}
            onValidationStateChange={onValidationStateChange}
            projectId={projectId}
            formValues={editToolDetail}
            formInitialValues={editToolDetail}
            onSave={handleSave}
            onSaveSuccess={onSaveSuccess}
            onSaveError={onSaveError}
            toolkitValidation={saveValidation.toolkitValidation}
            slots={formSlots}
          />
        </Grid>
      )}
      <Grid
        size={{ xs: 12, lg: !editToolDetail || isFullScreenChat ? 12 : 8 }}
        sx={rightGridItemSx}
        container
      >
        {toolkitId !== undefined && (
          <Box sx={historyButtonRowSx}>
            <ViewRunHistoryButton onShowHistory={handleShowHistory} />
          </Box>
        )}
        <TestPaneArea
          render={renderTestPane}
          projectId={projectId}
          toolkitId={toolkitId}
          editToolDetail={editToolDetail}
          applicationId={applicationId}
          isFullScreenChat={isFullScreenChat}
          onFullScreenChatChange={setIsFullScreenChat}
          onShowHistory={handleShowHistory}
        />
      </Grid>
    </Grid>
  );
}

const spinnerContainerSx: SxProps<Theme> = { height: '100%', width: '100%', display: 'flex', justifyContent: 'center', alignItems: 'center' };

const gridContainerSx: SxProps<Theme> = { height: '100%', maxHeight: '100%', paddingTop: '1rem', paddingBottom: '1.5rem', paddingLeft: '1.5rem', paddingRight: '1.5rem' };

const leftGridItemSx: SxProps<Theme> = { overflow: 'auto', maxHeight: '100%', height: '100%' };

const rightGridItemSx: SxProps<Theme> = { height: '100%', maxHeight: '100%' };

const historyButtonRowSx: SxProps<Theme> = { display: 'flex', justifyContent: 'flex-end', width: '100%' };
