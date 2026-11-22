import type { ReactNode } from 'react';
import { memo, useMemo } from 'react';

import CloseRoundedIcon from '@mui/icons-material/CloseRounded';
import HelpOutlineOutlinedIcon from '@mui/icons-material/HelpOutlineOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseBtn, BUTTON_VARIANTS } from '@/shared/ui/BaseBtn';

import { PipelineNodeDisplayNames, type PipelineNodeType } from '../../lib/flow-editor/constants/flowEditor.constants';
import { getNodeDocsLink, NODE_DOCS_LINK_REL } from '../../lib/flow-editor/constants/nodeDocs.constants';
import { NodeTypeIconTile } from '../add-node/NodeTypeIconTile';
import { PipelineNodeDescriptions } from '../add-node/nodeTypeDescriptions';

export interface NodeConfigPanelHeaderProps {
  readonly type: string;
  readonly onClose: () => void;
}

const rootSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  alignItems: 'center',
  gap: '0.75rem',
  padding: '0.75rem 1rem 0.75rem 1.5rem',
  borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}`,
  flexShrink: 0,
});

const textSectionSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  flex: 1,
  minWidth: 0,
};

const actionsSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: '0.25rem',
  flexShrink: 0,
};

const iconBtnSx: SxProps<Theme> = {
  minWidth: '1.75rem',
  width: '1.75rem',
  height: '1.75rem',
  padding: 0,
};

export const NodeConfigPanelHeader = memo(function NodeConfigPanelHeader({
  type,
  onClose,
}: NodeConfigPanelHeaderProps): ReactNode {
  const typeName = (PipelineNodeDisplayNames as Readonly<Record<string, string>>)[type] ?? type;
  const description = PipelineNodeDescriptions[type as PipelineNodeType];
  const docsLink = useMemo(() => getNodeDocsLink(type), [type]);

  return (
    <Box sx={rootSx} data-testid="pipeline-node-panel-header">
      <NodeTypeIconTile type={type} />
      <Box sx={textSectionSx}>
        <Typography variant="bodyMedium" color="text.secondary" noWrap data-testid="pipeline-node-panel-type">
          {typeName}
        </Typography>
        {description ? (
          <Tooltip title={description} placement="bottom-start">
            <Typography variant="bodySmall" color="text.primary" noWrap data-testid="pipeline-node-panel-description">
              {description}
            </Typography>
          </Tooltip>
        ) : null}
      </Box>
      <Box sx={actionsSx}>
        {docsLink ? (
          <Tooltip title={docsLink.tooltip} placement="top">
            <Box component="span">
              <IconButton
                component="a"
                size="small"
                href={docsLink.url}
                target="_blank"
                rel={NODE_DOCS_LINK_REL}
                aria-label={docsLink.tooltip}
                data-testid="pipeline-node-panel-docs"
                sx={iconBtnSx}
              >
                <HelpOutlineOutlinedIcon fontSize="small" />
              </IconButton>
            </Box>
          </Tooltip>
        ) : null}
        <Tooltip title={t('pipelines.nodeConfigPanel.close', 'Close')} placement="top">
          <Box component="span">
            <BaseBtn
              variant={BUTTON_VARIANTS.tertiary}
              onClick={onClose}
              aria-label={t('pipelines.nodeConfigPanel.closePanel', 'Close node panel')}
              data-testid="pipeline-node-panel-close"
              sx={iconBtnSx}
            >
              <CloseRoundedIcon fontSize="small" />
            </BaseBtn>
          </Box>
        </Tooltip>
      </Box>
    </Box>
  );
});
