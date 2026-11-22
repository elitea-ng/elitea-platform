import type { ReactNode } from 'react';
import { memo } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { PipelineNodeDisplayNames } from '../../lib/flow-editor/constants/flowEditor.constants';
import { NodeTypeIconTile } from '../add-node/NodeTypeIconTile';
import type { NodeCardV2Status } from './nodeCardV2.helpers';
import { NodeCardV2StatusIcon } from './NodeCardV2StatusIcon';

export interface NodeCardV2HeaderProps {
  readonly name: string;
  readonly type: string;
  readonly isEntrypoint?: boolean | undefined;
  readonly isDeprecated?: boolean | undefined;
  readonly status?: NodeCardV2Status | undefined;
  readonly issues?: readonly string[] | undefined;
  /** Optional badge/icon slot compatible with `NodeAdmissionIssues` or custom status badges. */
  readonly admissionBadge?: ReactNode;
}

const containerSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: '0.75rem',
  width: '100%',
  minWidth: 0,
};

const textSectionSx: SxProps<Theme> = {
  display: 'flex',
  flexDirection: 'column',
  flex: 1,
  minWidth: 0,
};

const nameTextSx: SxProps<Theme> = {
  whiteSpace: 'nowrap',
  textOverflow: 'ellipsis',
  overflow: 'hidden',
};

const rightSectionSx: SxProps<Theme> = {
  display: 'flex',
  justifyContent: 'flex-end',
  alignItems: 'center',
  gap: '0.375rem',
  flexShrink: 0,
};

const badgeChipSx: SxProps<Theme> = {
  height: '1.25rem',
};

export const NodeCardV2Header = memo(function NodeCardV2Header({
  name,
  type,
  isEntrypoint = false,
  isDeprecated = false,
  status = null,
  issues,
  admissionBadge,
}: NodeCardV2HeaderProps): ReactNode {
  const displayType = (PipelineNodeDisplayNames as Readonly<Record<string, string>>)[type] ?? type;

  return (
    <Box sx={containerSx} data-testid="pipeline-node-v2-header">
      <NodeTypeIconTile type={type} />
      <Box sx={textSectionSx}>
        <Typography variant="bodyMedium" color="text.secondary" sx={nameTextSx} data-testid="pipeline-node-v2-name">
          {name}
        </Typography>
        <Typography variant="bodySmall" color="text.primary" noWrap data-testid="pipeline-node-v2-type">
          {displayType}
        </Typography>
      </Box>
      <Box sx={rightSectionSx}>
        {isEntrypoint && (
          <Chip
            size="small"
            label={t('pipelines.nodeCardV2.entrypoint', 'Entrypoint')}
            sx={badgeChipSx}
            data-testid="pipeline-node-v2-entrypoint"
          />
        )}
        {isDeprecated && (
          <Chip
            size="small"
            color="warning"
            label={t('pipelines.nodeCardV2.deprecated', 'Deprecated')}
            sx={badgeChipSx}
            data-testid="pipeline-node-v2-deprecated"
          />
        )}
        {admissionBadge}
        <NodeCardV2StatusIcon status={status} issues={issues} />
      </Box>
    </Box>
  );
});
