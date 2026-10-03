import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { TypographyWithConditionalTooltip } from '@/shared/ui/TypographyWithConditionalTooltip';
import { AttachIcon } from '@/shared/ui/icons/attach-icon';

import type { AgentToolAssociation } from '../lib/types';

import { AgentPipelineVersionSelector } from './AgentPipelineVersionSelector';
import { BaseCardBody } from './BaseCardBody';
import { attachIconSx, attachmentButtonSx, contentBoxSx, titleRowSx, toolkitNameSx, variablesToggleCountSx, variablesToggleLabelSx, variablesToggleSx } from './ToolCard.styles';
import type { ToolCardVersionSelectorProps } from './ToolCard.types';

/**
 * `ToolCard`'s title row + body region, split out of `ToolCard.tsx` purely
 * to keep the `oxlint` `complexity` budget (≤12) — the JSX-embedded
 * conditionals this region owns (`isAgentOrPipeline && versionSelector`,
 * `hasVariables &&`, `!isAgentOrPipeline &&`) contributed directly to
 * `ToolCard`'s own function-body cyclomatic complexity when they lived
 * there (each `&&`/ternary in a function's OWN body counts toward it; a
 * JSX expression container is not a separate function scope, unlike a
 * `useMemo`/`useCallback` callback).
 */
export interface ToolCardBodyProps {
  readonly tool: AgentToolAssociation;
  readonly toolkitName: string;
  readonly isAttachmentToolkit: boolean;
  readonly isAgentOrPipeline: boolean;
  readonly versionSelector?: ToolCardVersionSelectorProps | undefined;
  readonly disabled?: boolean | undefined;
  /**
   * Whether to offer the "Show variables" toggle at all — the caller's
   * `tool.variables?.length` check AND its own "can an edit to them even be
   * persisted?" answer, folded into one boolean because the caller owns both
   * (and because computing them here costs `ToolCardBody` its last point of
   * `complexity` budget headroom).
   *
   * False WITHHOLDS the toggle even for a tool that carries variables: nothing
   * on this backend can store per-tool variables (#248 — see
   * `ToolCardProps.variables` and `AgentToolRow`'s module doc comment). Hidden,
   * not disabled, matching the legacy baseline's own omit-when-empty gate on
   * this same toggle.
   */
  readonly showVariablesToggle: boolean;
  readonly showVariables: boolean;
  readonly onToggleVariables: () => void;
  readonly showActions: boolean;
  readonly onClickShowActions: () => void;
}

export function ToolCardBody({ tool, toolkitName, isAttachmentToolkit, isAgentOrPipeline, versionSelector, disabled, showVariablesToggle, showVariables, onToggleVariables, showActions, onClickShowActions }: ToolCardBodyProps): ReactNode {
  return (
    <Box sx={contentBoxSx(isAgentOrPipeline)}>
      <Box sx={titleRowSx}>
        <TypographyWithConditionalTooltip
          title={toolkitName}
          placement="right"
          variant="bodyMedium"
          color="text.secondary"
          sx={toolkitNameSx}
        >
          {toolkitName}
        </TypographyWithConditionalTooltip>
        {isAttachmentToolkit && (
          <IconButton
            color="tertiary"
            size="small"
            disabled
            sx={attachmentButtonSx}
          >
            <Box
              component={AttachIcon}
              sx={attachIconSx}
            />
          </IconButton>
        )}
      </Box>

      {isAgentOrPipeline && versionSelector && (
        <AgentPipelineVersionSelector
          applicationVersionId={tool.settings?.application_version_id}
          disabled={disabled}
          versions={versionSelector.versions}
          isRefreshingVersions={versionSelector.isRefreshingVersions}
          onRefreshVersions={versionSelector.onRefreshVersions}
          isSwitchingVersion={versionSelector.isSwitchingVersion}
          onSelectVersion={versionSelector.onSelectVersion}
        />
      )}

      {showVariablesToggle && (
        <Box
          sx={variablesToggleSx}
          onClick={onToggleVariables}
        >
          <Typography
            component="span"
            variant="bodySmall"
            sx={variablesToggleLabelSx}
          >
            {showVariables ? t('agents.toolCard.hideVariables', 'Hide variables') : t('agents.toolCard.showVariables', 'Show variables')}
          </Typography>
          <Typography
            component="span"
            variant="bodySmall"
            sx={variablesToggleCountSx}
          >
            ({tool.variables?.length ?? 0})
          </Typography>
        </Box>
      )}

      {!isAgentOrPipeline && (
        <BaseCardBody
          tool={tool}
          onClickShowActions={onClickShowActions}
          showActions={showActions}
        />
      )}
    </Box>
  );
}
