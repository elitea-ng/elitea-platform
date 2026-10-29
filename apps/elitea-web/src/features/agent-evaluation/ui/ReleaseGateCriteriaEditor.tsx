import type { ReactNode } from 'react';

import DeleteOutlineIcon from '@mui/icons-material/DeleteOutlined';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import MenuItem from '@mui/material/MenuItem';
import TextField from '@mui/material/TextField';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import { MAX_GATE_CRITERIA, type CriterionDraft, type CriterionKind } from '../lib/releaseGateForm';
import type { EvalDimension } from '../model/types';

/**
 * The criteria of one gate suite: a dimension, a kind and a number per row.
 * `threshold` needs the dimension's mean score (0-100) to reach the number;
 * `no_worse_than` allows it to drop at most that many points below the
 * current release. At most five rows, one per dimension.
 */
export interface ReleaseGateCriteriaEditorProps {
  readonly criteria: readonly CriterionDraft[];
  readonly dimensions: readonly EvalDimension[];
  readonly disabled: boolean;
  readonly onChange: (next: readonly CriterionDraft[]) => void;
}

export function ReleaseGateCriteriaEditor({
  criteria,
  dimensions,
  disabled,
  onChange,
}: ReleaseGateCriteriaEditorProps): ReactNode {
  const update = (index: number, patch: Partial<CriterionDraft>): void =>
    onChange(criteria.map((criterion, at) => (at === index ? { ...criterion, ...patch } : criterion)));
  const used = new Set(criteria.map((criterion) => criterion.dimensionId));
  const nextDimension = dimensions.find((dimension) => !used.has(dimension.id))?.id ?? '';

  return (
    <Box
      sx={stackSx}
      data-testid="release-gate-criteria"
    >
      {criteria.map((criterion, index) => (
        <Box
          key={`${criterion.dimensionId}:${index}`}
          sx={rowSx}
          data-testid={`release-gate-criterion-${index}`}
        >
          <TextField
            select
            size="small"
            label={t('features.agentEvaluation.releaseGate.dimension', 'Dimension')}
            value={criterion.dimensionId}
            disabled={disabled}
            onChange={(event) => update(index, { dimensionId: event.target.value })}
            sx={{ minWidth: '12rem' }}
          >
            {dimensions.map((dimension) => (
              <MenuItem
                key={dimension.id}
                value={dimension.id}
                disabled={dimension.id !== criterion.dimensionId && used.has(dimension.id)}
              >
                {dimension.name}
              </MenuItem>
            ))}
          </TextField>
          <TextField
            select
            size="small"
            label={t('features.agentEvaluation.releaseGate.kind', 'Rule')}
            value={criterion.kind}
            disabled={disabled}
            onChange={(event) => update(index, { kind: event.target.value as CriterionKind })}
            sx={{ minWidth: '11rem' }}
          >
            <MenuItem value="threshold">{t('features.agentEvaluation.releaseGate.threshold', 'Score at least')}</MenuItem>
            <MenuItem value="no_worse_than">
              {t('features.agentEvaluation.releaseGate.noWorseThan', 'Drop at most')}
            </MenuItem>
          </TextField>
          <TextField
            size="small"
            type="number"
            label={
              criterion.kind === 'threshold'
                ? t('features.agentEvaluation.releaseGate.minScore', 'Min score (0-100)')
                : t('features.agentEvaluation.releaseGate.maxDrop', 'Max drop (points)')
            }
            value={criterion.value}
            disabled={disabled}
            onChange={(event) => update(index, { value: event.target.value })}
            slotProps={{ htmlInput: { min: 0, max: 100, 'data-testid': `release-gate-criterion-value-${index}` } }}
            sx={{ width: '10rem' }}
          />
          <IconButton
            size="small"
            disabled={disabled}
            aria-label={t('features.agentEvaluation.releaseGate.removeCriterion', 'Remove criterion')}
            onClick={() => onChange(criteria.filter((_, at) => at !== index))}
          >
            <DeleteOutlineIcon fontSize="small" />
          </IconButton>
        </Box>
      ))}
      <Box>
        <Button
          size="small"
          disabled={disabled || criteria.length >= MAX_GATE_CRITERIA || nextDimension === ''}
          onClick={() => onChange([...criteria, { dimensionId: nextDimension, kind: 'threshold', value: '' }])}
          data-testid="release-gate-add-criterion"
        >
          {t('features.agentEvaluation.releaseGate.addCriterion', 'Add criterion ({{count}} of {{max}})', {
            count: criteria.length,
            max: MAX_GATE_CRITERIA,
          })}
        </Button>
      </Box>
    </Box>
  );
}

const stackSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.5rem' };
const rowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: '0.5rem' };
