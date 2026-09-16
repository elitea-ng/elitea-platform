import type { ReactNode } from 'react';
import { useCallback } from 'react';

import Box from '@mui/material/Box';
import ButtonGroup from '@mui/material/ButtonGroup';
import Divider from '@mui/material/Divider';

import { useIsActiveParticipantBeingEdited } from '@/entities/participant';
import { t } from '@/shared/i18n';
import { PERMISSIONS } from '@/shared/lib/permissions';

import { useEditedParticipantId } from '../api/useEditedParticipantId';
import { useAgentEditorPanelFit } from '../lib/hooks/useAgentEditorPanelFit.hooks';
import { useCheckPermission } from '../lib/hooks/useCheckPermission.hooks';
import { useParticipantEntityIcon } from '../lib/hooks/useParticipantEntityIcon.hooks';

import { agentEditorPanelStyles } from './AgentEditorPanel.styles';
import {
  canEditParticipant,
  isDetailsLoading,
  isEditSettingsDisabled,
  isPipelineParticipant,
  isPublicParticipant,
  isSelectedVersionPublished,
  resolvePublicProjectId,
  resolveSelectedVersion,
  settingsTooltipTitle,
  switchEntityTooltip,
} from './AgentEditorPanel.derive';
import type { AgentEditorPanelProps } from './AgentEditorPanel.types';
import { AgentEditorPanelSkeleton } from './AgentEditorPanelSkeleton';
import { EditLlmSettingsButton } from './EditLlmSettingsButton';
import { EntitySwitchButton } from './EntitySwitchButton';
import { SettingsButton } from './SettingsButton';
import { SwitchToModelButton } from './SwitchToModelButton';
import { VariablesEditor } from './VariablesEditor';
import { VersionSelector } from './VersionSelector';

export type { AgentEditorPanelProps } from './AgentEditorPanel.types';

/**
 * Local module augmentation, scoped to this file's own need: `shared/brand/
 * theme.augment.d.ts` (out of this cluster's file scope — a shared brand/
 * theme file, not editable here) already adds `elitea` to `@mui/material/
 * Button`'s `ButtonPropsVariantOverrides`, but has no equivalent entry for
 * `@mui/material/ButtonGroup`'s sibling `ButtonGroupPropsVariantOverrides`.
 * Without it, restoring the baseline's `<ButtonGroup variant="elitea" ...>`
 * below (see this component's own doc comment, "ButtonGroup restoration")
 * fails `tsc` with `Type '"elitea"' is not assignable to type
 * OverridableStringUnion<'contained'|'outlined'|'text', ...>`. TypeScript's
 * declaration-merging is file-location-agnostic (this is the same mechanism
 * `theme.augment.d.ts` itself uses), so this is a real, typed fix, not a
 * cast — but the canonical home for it is alongside the existing `Button`
 * augmentation in `theme.augment.d.ts`. Flagged as a follow-up in this
 * fix's own report rather than edited there, per this cluster's file-scope
 * boundary.
 */
declare module '@mui/material/ButtonGroup' {
  interface ButtonGroupPropsVariantOverrides {
    elitea: true;
  }
}

/**
 * Port of `apps/elitea-ui/src/[fsd]/features/chat/ui/chat-input/
 * AgentEditorPanel.jsx` (unit C3, "chat-input" cluster — composed inside
 * `NewChatInput.tsx`, only when `activeParticipant.entityName` is
 * `'application'`/`'pipeline'`). Split across `AgentEditorPanel.types.ts`
 * (props), `AgentEditorPanel.derive.ts` (pure derived state),
 * `AgentEditorPanel.styles.ts` (the sx factory), and several small
 * sub-components (`EntitySwitchButton`/`SettingsButton`/
 * `SwitchToModelButton`/`AgentEditorEntityIcon`/`AgentEditorPanelSkeleton`)
 * — purely to keep this file, and every function in it, under the §3.5
 * budgets (≤400 lines, ≤12 cyclomatic complexity, ≤12 component props).
 *
 * Reuses (does not re-derive): `entities/version`'s `selectDefaultVersion`
 * (see `AgentEditorPanel.derive.ts`'s `resolveSelectedVersion` for the
 * disclosed narrowing versus the baseline's own inline chain).
 *
 * **`onRefresh` wiring — disclosed baseline bug fix, not a reproduction**:
 * the baseline receives `onRefreshParticipantDetails` as a prop but never
 * reads it, and never passes an `onRefresh` prop to `VersionSelector` —
 * meaning `VersionSelector`'s own refresh-versions button (`onRefresh &&
 * (...)`) is baseline-DEAD code, never rendered in production. Since the
 * prop was already fully plumbed down from `NewChatInput.jsx` and simply
 * never connected, this port wires it through (`version.onRefresh` ->
 * `VersionSelector`'s `onRefresh`) rather than reproducing the drop.
 *
 * **`useApplicationChatSwitchVersion` — disclosed judgment call, NOT
 * ported here.** The task brief asks whether the old sibling hook
 * `useApplicationChatSwitchVersion.js`'s persistence orchestration
 * (writing the newly-selected version's settings back onto the active
 * participant via `updateParticipantSettings`) belongs in `NewChatInput
 * .tsx` or as an injected callback. Reading the baseline's own call graph:
 * that hook is invoked by `useApplicationChat.hooks.js`, itself called
 * from a NewChat/ChatBox-level page component — TWO LAYERS above
 * `NewChatInput.jsx`, never from `NewChatInput.jsx`/`AgentEditorPanel.jsx`/
 * `VersionSelector.jsx` themselves. It also needs state
 * (`activeConversation`, a fetched `applicationVersionDetails` query,
 * `setActiveParticipant`) that does not exist anywhere in `NewChatInput`'s
 * current prop surface, and inventing it would exceed this unit's remit.
 * `version.onSelect` therefore stays a pure pass-through callback, exactly
 * matching the baseline's own `VersionSelector.onSelect={onSelectVersion}`
 * — the real persistence orchestration remains a future composition-root
 * (C6-equivalent) concern, wired exactly where the baseline wires it.
 *
 * Known, disclosed gaps (see `AgentEditorPanel.derive.ts`'s own comments):
 * `usePublicProjectAccessCheck` has no port (`canEdit` reduces to `!isPublic
 * && hasEditPermission`, never more permissive than the baseline).
 * `useParticipantEntityIcon`/`useAgentEditorPanelFit`/`useCheckPermission`
 * are small local ports in `../lib/hooks/` (no shared home exists).
 * `EntityIcon` is a small local `AgentEditorEntityIcon` (the shared
 * `features/agents/ui/EntityIcon.tsx` is scoped/unexported for a different
 * call site and illegal to reach anyway).
 */
export function AgentEditorPanel(props: AgentEditorPanelProps): ReactNode {
  const {
    activeParticipant,
    participantDetails,
    disabled,
    disableSwitchToModel,
    isEditorDirty,
    onClickParticipant,
    onSwitchToModel,
    version,
    variablesEditor,
    editorNav,
    onEditLlmSettings,
  } = props;

  const { checkPermission } = useCheckPermission();
  const { containerRef, isSmallView } = useAgentEditorPanelFit();
  const entityIcon = useParticipantEntityIcon(activeParticipant);
  const editedParticipantId = useEditedParticipantId();
  const styles = agentEditorPanelStyles(isSmallView);

  const isPipeline = isPipelineParticipant(activeParticipant);
  const detailsLoading = isDetailsLoading(participantDetails, activeParticipant);
  const isPublic = isPublicParticipant(activeParticipant, resolvePublicProjectId());
  const hasEditPermission = checkPermission(PERMISSIONS.applications.update);
  const canEdit = canEditParticipant(isPublic, hasEditPermission);
  const isBeingEdited = useIsActiveParticipantBeingEdited(activeParticipant, editedParticipantId);

  const selectedVersion = resolveSelectedVersion(participantDetails?.versions, version.selectedVersionId);
  const versionPublished = isSelectedVersionPublished(selectedVersion);
  const settingsDisabled = isEditSettingsDisabled(isPublic, versionPublished);

  const onCloseEditor = useCallback(() => {
    if (isPipeline) editorNav.onClosePipelineEditor?.();
    else editorNav.onCloseAgentEditor?.();
  }, [isPipeline, editorNav]);

  const onClickAgentEditor = useCallback(() => {
    if (!activeParticipant) return;
    if (isPipeline) editorNav.onShowPipelineEditor?.(activeParticipant);
    else editorNav.onShowAgentEditor?.(activeParticipant);
  }, [isPipeline, editorNav, activeParticipant]);

  if (detailsLoading) {
    return (
      <AgentEditorPanelSkeleton
        containerRef={containerRef}
        disabled={disabled || disableSwitchToModel}
        onSwitchToModel={onSwitchToModel}
        styles={styles}
      />
    );
  }

  return (
    <Box
      ref={containerRef}
      sx={styles.outerContainer}
    >
      <ButtonGroup
        variant="elitea"
        disableElevation
        color="secondary"
        disabled={disabled}
        aria-label={t('chatInput.agentEditorPanel.modelSelectorMenu', 'Model Selector Menu')}
        sx={styles.buttonRow}
      >
        <EntitySwitchButton
          tooltip={switchEntityTooltip(isPipeline)}
          onClick={onClickParticipant}
          iconUrl={entityIcon?.url}
          isPipeline={isPipeline}
          isSmallView={isSmallView}
          participantName={participantDetails?.name}
          styles={styles}
        />

        {!!participantDetails?.versions?.length && (
          <>
            <Divider orientation="vertical" />
            <VersionSelector
              selectedVersion={selectedVersion}
              versions={participantDetails.versions}
              onSelect={version.onSelect}
              onCloseEditor={onCloseEditor}
              isEditorDirty={isEditorDirty}
              onShowVersionChangeAlert={version.onShowVersionChangeAlert}
              isSmallView={isSmallView}
              onRefresh={version.onRefresh}
            />
          </>
        )}

        {!!variablesEditor.variables.length && (
          <>
            <Divider orientation="vertical" />
            <VariablesEditor
              variables={variablesEditor.variables}
              onChange={variablesEditor.onChange}
              isSmallView={isSmallView}
            />
          </>
        )}

        <Divider orientation="vertical" />

        <SettingsButton
          tooltip={settingsTooltipTitle(isPipeline, versionPublished, canEdit)}
          onClick={onClickAgentEditor}
          disabled={Boolean(disabled) || settingsDisabled}
          isBeingEdited={isBeingEdited}
          canEdit={canEdit}
          styles={styles}
        />

        {/*
         * A14 (ELITEA-0386): per-participant LLM settings, independent of
         * the full agent/pipeline editor `SettingsButton` opens above (that
         * button's own gate — `settingsDisabled` — is about the CANVAS
         * editor, not this).
         */}
        <EditLlmSettingsButton
          onEditLlmSettings={onEditLlmSettings}
          canEdit={canEdit}
          disabled={Boolean(disabled)}
        />
      </ButtonGroup>

      <SwitchToModelButton
        disabled={disabled || disableSwitchToModel}
        onSwitchToModel={onSwitchToModel}
      />
    </Box>
  );
}
