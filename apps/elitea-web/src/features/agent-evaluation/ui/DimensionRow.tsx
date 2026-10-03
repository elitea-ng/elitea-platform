/**
 * One row of the dimension library.
 *
 * A `platform`-tier row is READ-ONLY: it is materialised from a platform
 * catalogue rather than authored here, and the server refuses to update or
 * delete it. Rendering the controls for it would offer an action that always
 * fails.
 */
import type { ReactNode } from 'react';

import DeleteIcon from '@mui/icons-material/Delete';
import EditIcon from '@mui/icons-material/Edit';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { EVAL_TIER, type EvalDimension } from '../model/types';

const rowSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: '1rem',
  padding: '0.75rem 0',
  borderBottom: 1,
  borderColor: 'divider',
};
const textSx: SxProps<Theme> = { flex: 1, minWidth: 0 };

export interface DimensionRowProps {
  readonly dimension: EvalDimension;
  readonly canEdit: boolean;
  readonly canDelete: boolean;
  readonly onEdit: (dimension: EvalDimension) => void;
  readonly onDelete: (dimension: EvalDimension) => void;
}

export function DimensionRow(props: DimensionRowProps): ReactNode {
  const { dimension, canEdit, canDelete, onEdit, onDelete } = props;
  const isReadOnly = dimension.tier === EVAL_TIER.platform;

  return (
    <Box
      sx={rowSx}
      data-testid={`evaluation-dimension-row-${dimension.id}`}
    >
      <Box sx={textSx}>
        <Typography variant="bodyMedium" component="p">{dimension.name}</Typography>
        <Typography
          variant="bodyMedium"
          component="p"
          color="text.secondary"
        >
          {`${dimension.allowed_engines.join(', ')} · ${dimension.scale_min}-${dimension.scale_max} · ${dimension.polarity}`}
        </Typography>
      </Box>
      {canEdit && !isReadOnly && (
        <IconButton
          aria-label={t('features.agentEvaluation.editDimension', 'Edit dimension')}
          data-testid={`evaluation-dimension-edit-${dimension.id}`}
          onClick={() => onEdit(dimension)}
        >
          <EditIcon fontSize="small" />
        </IconButton>
      )}
      {canDelete && !isReadOnly && (
        <IconButton
          aria-label={t('features.agentEvaluation.deleteDimension', 'Delete dimension')}
          data-testid={`evaluation-dimension-delete-${dimension.id}`}
          onClick={() => onDelete(dimension)}
        >
          <DeleteIcon fontSize="small" />
        </IconButton>
      )}
    </Box>
  );
}
