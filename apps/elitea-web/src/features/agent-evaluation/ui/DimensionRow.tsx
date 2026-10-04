/**
 * One row of the dimension library.
 *
 * A `platform`-tier row is READ-ONLY: it is materialised from a platform
 * catalogue rather than authored here, and the server refuses to update or
 * delete it. Rendering the controls for it would offer an action that always
 * fails.
 *
 * LAYOUT (legacy issue 6694). The name is ellipsized and keeps at least ten
 * characters visible. When the row is too narrow for the name and the tags,
 * the tags wrap to a new line below the name instead of squeezing it. Each
 * tag has a tooltip that says what it means, shown after a two-second hover.
 */
import type { ReactNode } from 'react';

import DeleteIcon from '@mui/icons-material/Delete';
import EditIcon from '@mui/icons-material/Edit';
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { TypographyWithConditionalTooltip } from '@/shared/ui/TypographyWithConditionalTooltip';

import { EVAL_ENGINE, EVAL_TIER, type EvalDimension, type EvalEngine } from '../model/types';

/** The tag tooltips wait this long, so a pointer that crosses the row does not open them. */
const DIMENSION_TAG_TOOLTIP_DELAY_MS = 2000;

const rowSx: SxProps<Theme> = {
  display: 'flex',
  alignItems: 'center',
  gap: '1rem',
  padding: '0.75rem 0',
  borderBottom: 1,
  borderColor: 'divider',
};
const bodySx: SxProps<Theme> = {
  flex: 1,
  minWidth: 0,
  display: 'flex',
  flexWrap: 'wrap',
  alignItems: 'center',
  columnGap: '0.75rem',
  rowGap: '0.25rem',
};
// `10ch` is the floor the issue asks for: the name shrinks to ten visible
// characters, and below that the tags move to the next line.
const nameSx: SxProps<Theme> = { flex: '1 1 10ch', minWidth: '10ch' };
const tagsSx: SxProps<Theme> = { display: 'flex', flexWrap: 'wrap', gap: '0.25rem' };

function engineLabel(engine: EvalEngine): string {
  switch (engine) {
    case EVAL_ENGINE.ai:
      return t('features.agentEvaluation.engine.ai', 'AI');
    case EVAL_ENGINE.human:
      return t('features.agentEvaluation.engine.human', 'Human');
    case EVAL_ENGINE.code:
      return t('features.agentEvaluation.engine.code', 'Code');
  }
}

function engineTooltip(engine: EvalEngine): string {
  switch (engine) {
    case EVAL_ENGINE.ai:
      return t(
        'features.agentEvaluation.tags.engineAiTooltip',
        'This dimension is evaluated using the suite’s judge model.',
      );
    case EVAL_ENGINE.human:
      return t('features.agentEvaluation.tags.engineHumanTooltip', 'This dimension requires manual review.');
    case EVAL_ENGINE.code:
      return t(
        'features.agentEvaluation.tags.engineCodeTooltip',
        'This dimension is evaluated using Python validation logic.',
      );
  }
}

interface TagProps {
  readonly label: string;
  readonly tooltip: string;
  readonly testId: string;
}

function DimensionTag({ label, tooltip, testId }: TagProps): ReactNode {
  return (
    <Tooltip title={tooltip} enterDelay={DIMENSION_TAG_TOOLTIP_DELAY_MS} enterNextDelay={DIMENSION_TAG_TOOLTIP_DELAY_MS}>
      <Chip size="small" variant="outlined" label={label} data-testid={testId} />
    </Tooltip>
  );
}

function DimensionTags({ dimension }: { readonly dimension: EvalDimension }): ReactNode {
  const hasTarget = dimension.default_target !== null && dimension.default_target_operator !== '';
  return (
    <Box sx={tagsSx}>
      {dimension.allowed_engines.map((engine) => (
        <DimensionTag
          key={engine}
          label={engineLabel(engine)}
          tooltip={engineTooltip(engine)}
          testId={`evaluation-dimension-tag-engine-${engine}-${dimension.id}`}
        />
      ))}
      {hasTarget && (
        <DimensionTag
          label={t('features.agentEvaluation.tags.target', 'Target {{operator}} {{value}}', {
            operator: dimension.default_target_operator,
            value: String(dimension.default_target),
          })}
          tooltip={t(
            'features.agentEvaluation.tags.targetTooltip',
            'The score or rating that must satisfy the selected success criterion for this dimension to pass.',
          )}
          testId={`evaluation-dimension-tag-target-${dimension.id}`}
        />
      )}
      <DimensionTag
        label={t('features.agentEvaluation.tags.importance', 'Weight {{weight}}', {
          weight: String(dimension.default_weight),
        })}
        tooltip={t(
          'features.agentEvaluation.tags.importanceTooltip',
          'Indicates how significant this dimension is when interpreting the overall evaluation result.',
        )}
        testId={`evaluation-dimension-tag-importance-${dimension.id}`}
      />
    </Box>
  );
}

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
      <Box sx={bodySx}>
        <Box sx={nameSx}>
          <TypographyWithConditionalTooltip
            title={dimension.name}
            placement="top"
            variant="bodyMedium"
            data-testid={`evaluation-dimension-name-${dimension.id}`}
          >
            {dimension.name}
          </TypographyWithConditionalTooltip>
        </Box>
        <DimensionTags dimension={dimension} />
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
