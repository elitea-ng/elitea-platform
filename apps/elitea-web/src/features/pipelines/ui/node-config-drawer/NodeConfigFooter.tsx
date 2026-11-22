import type { ReactNode } from 'react';
import { memo } from 'react';

import ContentCopyOutlinedIcon from '@mui/icons-material/ContentCopyOutlined';
import DeleteOutlineRoundedIcon from '@mui/icons-material/DeleteOutlineRounded';
import Box from '@mui/material/Box';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn, BUTTON_VARIANTS } from '@/shared/ui/BaseBtn';
import { AiSparkleIcon } from '@/shared/ui/icons/ai-sparkle-icon';

import { PipelineNodeTypes } from '../../lib/flow-editor/constants/flowEditor.constants';

export interface NodeConfigFooterProps {
  readonly id: string;
  readonly type: string;
  readonly isEntrypoint: boolean;
  readonly canMakeEntrypoint?: boolean | undefined;
  readonly canDuplicate?: boolean | undefined;
  readonly disabled?: boolean | undefined;
  readonly onMakeEntrypoint?: ((id: string) => void) | undefined;
  readonly onDuplicate?: ((id: string) => void) | undefined;
  readonly onDelete?: ((id: string) => void) | undefined;
}

const rootSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'space-between',
  padding: '1rem 1.5rem',
  borderTop: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  flexShrink: 0,
  gap: '0.5rem',
});

const entrypointStateSx: SxProps<Theme> = (theme) => ({
  display: 'inline-flex',
  alignItems: 'center',
  height: '1.75rem',
  padding: '0 1rem',
  boxSizing: 'border-box',
  borderRadius: theme.vars.shape.radiusLg,
  border: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  cursor: 'default',
});

const actionsSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: '0.5rem',
};

const iconBtnSx: SxProps<Theme> = {
  minWidth: '2rem',
  width: '2rem',
  height: '2rem',
  padding: 0,
};

const iconStyle = { width: '1rem', height: '1rem' };

function EntrypointSlot({
  id,
  type,
  isEntrypoint,
  canMakeEntrypoint,
  disabled,
  onMakeEntrypoint,
}: {
  readonly id: string;
  readonly type: string;
  readonly isEntrypoint: boolean;
  readonly canMakeEntrypoint: boolean;
  readonly disabled: boolean;
  readonly onMakeEntrypoint: ((id: string) => void) | undefined;
}): ReactNode {
  if (isEntrypoint) {
    return (
      <Box sx={entrypointStateSx} data-testid="pipeline-node-panel-entrypoint-state">
        <Typography variant="labelSmall" color="text.secondary">
          {t('pipelines.nodeConfigPanel.entrypoint', 'Entrypoint')}
        </Typography>
      </Box>
    );
  }
  const canBeEntrypoint =
    canMakeEntrypoint &&
    type !== PipelineNodeTypes.Condition &&
    type !== PipelineNodeTypes.Decision;
  if (!canBeEntrypoint || !onMakeEntrypoint) return null;
  return (
    <BaseBtn
      variant={BUTTON_VARIANTS.secondary}
      disabled={disabled}
      onClick={() => onMakeEntrypoint(id)}
      data-testid="pipeline-node-panel-make-entrypoint"
    >
      {t('pipelines.nodeConfigPanel.makeEntrypoint', 'Make Entrypoint')}
    </BaseBtn>
  );
}

/**
 * Footer for the Wave U2 node config panel:
 * - Entrypoint badge / "Make Entrypoint" button
 * - Disabled "Build with AI" ("Coming soon") button per Wave U1/U2 Q2 product ruling
 * - Duplicate and Delete node actions
 */
export const NodeConfigFooter = memo(function NodeConfigFooter(props: NodeConfigFooterProps): ReactNode {
  const { id, type, isEntrypoint, onMakeEntrypoint, onDuplicate, onDelete } = props;
  const canDuplicate = props.canDuplicate ?? true;
  const disabled = props.disabled ?? false;

  const duplicateTooltip = canDuplicate
    ? t('pipelines.nodeConfigPanel.duplicate', 'Duplicate')
    : t('pipelines.nodeConfigPanel.deprecatedDuplicateTooltip', "Deprecated nodes can't be duplicated");

  return (
    <Box sx={rootSx} data-testid="pipeline-node-panel-footer">
      <Box>
        <EntrypointSlot
          id={id}
          type={type}
          isEntrypoint={isEntrypoint}
          canMakeEntrypoint={props.canMakeEntrypoint ?? true}
          disabled={disabled}
          onMakeEntrypoint={onMakeEntrypoint}
        />
      </Box>
      <Box sx={actionsSx}>
        <Tooltip title={t('pipelines.nodeConfigPanel.comingSoon', 'Coming soon')} placement="top">
          <Box component="span">
            <BaseBtn
              variant={BUTTON_VARIANTS.special}
              disabled
              aria-label={t('pipelines.nodeConfigPanel.buildWithAi', 'Build with AI')}
              data-testid="pipeline-node-panel-build-with-ai"
              sx={iconBtnSx}
            >
              <AiSparkleIcon style={iconStyle} />
            </BaseBtn>
          </Box>
        </Tooltip>
        {onDuplicate ? (
          <Tooltip title={duplicateTooltip} placement="top">
            <Box component="span">
              <BaseBtn
                variant={BUTTON_VARIANTS.secondary}
                disabled={disabled || !canDuplicate}
                onClick={() => onDuplicate(id)}
                aria-label={t('pipelines.nodeConfigPanel.duplicateNode', 'Duplicate node')}
                data-testid="pipeline-node-panel-duplicate"
                sx={iconBtnSx}
              >
                <ContentCopyOutlinedIcon fontSize="small" />
              </BaseBtn>
            </Box>
          </Tooltip>
        ) : null}
        {onDelete ? (
          <Tooltip title={t('pipelines.nodeConfigPanel.delete', 'Delete')} placement="top">
            <Box component="span">
              <BaseBtn
                variant={BUTTON_VARIANTS.secondary}
                disabled={disabled}
                onClick={() => onDelete(id)}
                aria-label={t('pipelines.nodeConfigPanel.deleteNode', 'Delete node')}
                data-testid="pipeline-node-panel-delete"
                sx={iconBtnSx}
              >
                <DeleteOutlineRoundedIcon fontSize="small" />
              </BaseBtn>
            </Box>
          </Tooltip>
        ) : null}
      </Box>
    </Box>
  );
});
