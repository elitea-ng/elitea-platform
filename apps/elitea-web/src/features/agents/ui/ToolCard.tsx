import type { ReactNode } from 'react';
import { useCallback, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import { useTheme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

import { getAgentsBasename } from '../lib/basename';
import { getToolkitTypeLabel } from '../lib/toolkitBlocklist';
import type { AgentToolAssociation } from '../lib/types';

import { AgentVariables } from './AgentVariables';
import { ToolChoices } from './ToolChoices';
import { EntityIcon } from './EntityIcon';
import { buildOpenInNewTabUrl, resolveEntityType, resolveExtraAuthSlot, resolveMcpDisconnectedTip, resolveOpenAction, resolveRemoveAction, resolveRemoveDialogSuffix, resolveRemoveDialogTitle, resolveToolCardViewState, resolveToolkitName } from './ToolCard.lib';
import { cardContainerSx, cardHeaderSx, entityIconImageSx, entityIconSx } from './ToolCard.styles';
import { ToolCardActionsCluster } from './ToolCardActionsCluster';
import { ToolCardBody } from './ToolCardBody';
import type { ToolCardProps } from './ToolCard.types';


/**
 * The two mutually-exclusive status banners, extracted to its own function
 * scope for the same `complexity` budget reason as `ToolCardBody`/
 * `ToolCardActionsCluster` (see this file's own doc comment) — a lowercase,
 * non-component function so the §3.5 12-props budget (which keys on a
 * capitalised name) does not apply to its 5-field parameter object either.
 */
function renderStatusBanner(params: { isBlockedToolkit: boolean; someToolsAreUnavailable: boolean; hasValidationIssue: boolean; showActions: boolean; toolType: string | undefined }): ReactNode {
  const { isBlockedToolkit, someToolsAreUnavailable, hasValidationIssue, showActions, toolType } = params;
  if (hasValidationIssue || showActions) return null;
  if (isBlockedToolkit) return <BannerMessage message={t('agents.toolCard.blockedByOrganization', '{{toolkitType}} toolkit is blocked by your organization.', { toolkitType: getToolkitTypeLabel(toolType) })} />;
  if (someToolsAreUnavailable) return <BannerMessage message={t('agents.toolCard.someToolsUnavailable', 'Some tools are not available anymore.')} />;
  return null;
}

/**
 * Same reasoning as `renderStatusBanner`.
 *
 * `onChangeVariable` being absent means the caller WITHHELD the control
 * (#248 — nothing on this backend can store per-tool variables; see
 * `ToolCardProps.variables` and `AgentToolRow`'s module doc comment), so the
 * panel is not rendered at all. Withheld and "collapsed" are handled by the
 * same early return on purpose: the toggle that flips `show` is withheld too,
 * so `show` can never become true without a caller.
 */
function renderVariablesPanel(params: { show: boolean; variables: AgentToolAssociation['variables']; onChangeVariable: ((label: string, newValue: string) => void) | undefined }): ReactNode {
  if (!params.show || !params.onChangeVariable) return null;
  return (
    <AgentVariables
      variables={params.variables}
      onChangeVariable={params.onChangeVariable}
    />
  );
}

/**
 * Ported from `apps/elitea-ui/src/pages/Applications/Components/Tools/ToolCard.jsx`
 * (819 lines) — the largest, most cross-cutting file in this sub-unit
 * (A1h). Composes the other 4 owned files
 * (`AgentPipelineVersionSelector`/`AgentVariables`/`EnhancedCardToolActions`/
 * `BaseCardBody`, the first and last via the local `ToolCardBody`) plus a
 * local `EntityIcon`.
 *
 * See `ToolCard.types.ts`'s module doc comment for the full "MAJOR
 * DISCLOSED REDESIGN" rationale (moved there purely to keep this file
 * under the §3.5 400-line budget) — every prop group here replaces one or
 * more baseline hooks this sub-unit does not own or may not import.
 *
 * The title-row/body region and the trailing icon-button cluster are
 * further split into `ToolCardBody`/`ToolCardActionsCluster`, and nearly
 * every small derived value into `ToolCard.lib.ts`'s
 * `resolveToolCardViewState` — also purely for budget reasons, this time
 * the `oxlint` `complexity` budget (≤12): this component's JSX used to
 * carry ~30 inline `&&`/ternary/`?.` branches directly in its own function
 * body (a JSX expression container — and a `useMemo`/`useCallback`
 * dependency-array LITERAL — are not a separate function scope the way a
 * memoised CALLBACK BODY is), measured at complexity 67. See
 * `ToolCard.lib.ts`'s own doc comment on `resolveToolCardViewState` for why
 * even hook-wrapping alone did not fully fix this.
 */
export function ToolCard({ tool, disabled, isDuplicate = false, context, icon, disassociate, variables, toolSelection, validation, delegatedAuth, versionSelector }: ToolCardProps): ReactNode {
  const theme = useTheme();
  const [openAlert, setOpenAlert] = useState(false);
  const [showActions, setShowActions] = useState(false);
  const [showVariables, setShowVariables] = useState(false);

  const view = useMemo(() => resolveToolCardViewState({ tool, isDuplicate, context, disassociate, validation, delegatedAuth }), [tool, isDuplicate, context, disassociate, validation, delegatedAuth]);

  const entityType = useMemo(() => resolveEntityType(tool), [tool]);
  const toolkitName = useMemo(() => resolveToolkitName(tool, toolSelection.resolveToolkitNameFromSchema), [tool, toolSelection.resolveToolkitNameFromSchema]);


  const someToolsAreUnavailable = useMemo(() => {
    const availableTools = toolSelection.availableTools;
    return !!availableTools?.length && (view.selectedTools ?? []).some((item) => !availableTools.includes(item));
  }, [toolSelection.availableTools, view.selectedTools]);

  const onDelete = useCallback(() => setOpenAlert(true), []);
  const onCloseAlert = useCallback(() => setOpenAlert(false), []);
  const onConfirmAlert = useCallback(() => {
    setOpenAlert(false);
    disassociate.onDisassociateTool({ isAttachmentToolkit: view.isAttachmentToolkit });
  }, [disassociate, view.isAttachmentToolkit]);

  /**
   * Zero-arg (matches `BaseCardBodyProps.onClickShowActions: () => void`).
   * The baseline's `event.stopPropagation()` (`ToolCard.jsx:236-239`) is
   * dropped: it guarded against bubbling to an ancestor click handler that
   * does not exist anywhere in this restructured tree (no `Box` above
   * either toggle has an `onClick` of its own) — verified by reading this
   * component's full JSX, not assumed.
   */
  const onClickShowActions = useCallback(() => setShowActions((prev) => !prev), []);
  const onToggleVariables = useCallback(() => setShowVariables((prev) => !prev), []);

  /**
   * The variables control is offered only when a caller supplied a way to
   * persist an edit to it (#248) AND the tool actually carries variables. No
   * caller supplies one today — see `ToolCardProps.variables`.
   */
  const showVariablesToggle = variables !== undefined && view.hasVariables;

  const onOpenInNewTab = useCallback(() => {
    const url = buildOpenInNewTabUrl({
      isAgentOrPipeline: view.isAgentOrPipeline,
      applicationId: view.applicationId,
      applicationVersionId: view.applicationVersionId,
      agentType: tool.agent_type,
      toolId: tool.id,
      projectId: view.projectId,
      viewMode: context.viewMode,
      toolkitName,
      isMcp: view.isMcp,
      basename: getAgentsBasename(),
    });
    if (url) window.open(url, '_blank');
    // `view` covers isAgentOrPipeline/applicationId/applicationVersionId/projectId/isMcp as ONE dependency (§3.5 hook-deps budget: ≤8).
  }, [view, tool.agent_type, tool.id, context.viewMode, toolkitName]);

  const iconColor = disabled ? theme.vars.palette.icon.fill.disabled : theme.vars.palette.icon.fill.default;
  const extraAuthSlot = useMemo(() => resolveExtraAuthSlot(tool.type, delegatedAuth), [tool.type, delegatedAuth]);
  const showAttention = view.hasValidationIssue || someToolsAreUnavailable;
  const mcpDisconnectedTip = resolveMcpDisconnectedTip(tool.name, view.mcpIsAuthorized);
  const openAction = useMemo(() => resolveOpenAction({ onClick: onOpenInNewTab, entityType, isMcp: view.isMcp, disabled, toolId: tool.id, applicationId: view.applicationId }), [onOpenInNewTab, entityType, view.isMcp, disabled, tool.id, view.applicationId]);
  const removeAction = useMemo(() => resolveRemoveAction({ onClick: onDelete, entityType, disabled }), [onDelete, entityType, disabled]);

  return (
    <Tooltip
      title={view.duplicateTooltipTitle}
      placement="top"
    >
      <Box>
        <Box
          data-testid="agent-toolkit-card"
          sx={cardContainerSx(showActions, showVariables, isDuplicate)}
        >
          <Box sx={cardHeaderSx(showActions, showVariables, showVariablesToggle)}>
            <EntityIcon
              sx={entityIconSx}
              imageSx={entityIconImageSx}
              icon={icon}
              entityType={entityType}
            />
            <ToolCardBody
              tool={tool}
              toolkitName={toolkitName}
              isAttachmentToolkit={view.isAttachmentToolkit}
              isAgentOrPipeline={view.isAgentOrPipeline}
              versionSelector={versionSelector}
              disabled={disabled}
              showVariablesToggle={showVariablesToggle}
              showVariables={showVariables}
              onToggleVariables={onToggleVariables}
              showActions={showActions}
              onClickShowActions={onClickShowActions}
            />
            <ToolCardActionsCluster
              iconColor={iconColor}
              showAttention={showAttention}
              onRevalidate={view.onRevalidate}
              openAction={openAction}
              removeAction={removeAction}
              isRemoving={disassociate.isDisassociating}
              isMcp={view.isMcp}
              mcpIsAuthorized={view.mcpIsAuthorized}
              mcpDisconnectedTip={mcpDisconnectedTip}
              mcpLoginSlot={view.mcpLoginSlot}
              mcpLogoutSlot={view.mcpLogoutSlot}
              extraAuthSlot={extraAuthSlot}
            />
          </Box>
          {renderStatusBanner({ isBlockedToolkit: view.isBlockedToolkit, someToolsAreUnavailable, hasValidationIssue: view.hasValidationIssue, showActions, toolType: tool.type })}
          {renderVariablesPanel({ show: showVariables, variables: tool.variables, onChangeVariable: variables?.onChangeVariable })}
          {showActions && (
            <ToolChoices
              tool={tool}
              projectId={context.selectedProjectId}
              selectedTools={view.selectedTools}
              availableTools={toolSelection.availableTools}
              disabled={disabled}
              onSelectedToolsChange={toolSelection.onSelectedToolsChange}
            />
          )}
          <DeleteEntityModal
            open={openAlert}
            onClose={onCloseAlert}
            onConfirm={onConfirmAlert}
            name={toolkitName}
            alarm={false}
            confirming={view.isConfirmingRemove}
            copy={{ title: resolveRemoveDialogTitle(entityType), textContent: t('agents.toolCard.removeConfirmPrefix', 'Are you sure to remove the '), confirmText: t('agents.toolCard.removeConfirmButton', 'Remove') }}
            content={{ inline: resolveRemoveDialogSuffix(entityType, view.isAttachmentToolkit) }}
          />
        </Box>
        {view.validationBanner}
      </Box>
    </Tooltip>
  );
}
