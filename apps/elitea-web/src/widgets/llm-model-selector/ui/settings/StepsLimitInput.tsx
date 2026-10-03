import { memo, type ChangeEvent } from 'react';

import { Box, TextField, Typography } from '@mui/material';

import { DEFAULT_STEPS_LIMIT } from '@/shared/lib/constants';
import { MAX_STEP_LIMIT, MIN_STEP_LIMIT } from '@/shared/lib/limits';
import { parseValueToIntNumber } from '@/shared/lib/number';
import { t } from '@/shared/i18n';

interface StepsLimitInputProps {
  value: number;
  onChange: (value: number) => void;
}

/** Steps limit numeric input. */
export const StepsLimitInput = memo(({ value, onChange }: StepsLimitInputProps) => {
  const handleChange = (e: ChangeEvent<HTMLInputElement>) => {
    const parsed = parseValueToIntNumber(e.target.value);
    if (parsed === '') return;
    onChange(Math.min(Math.max(parsed, MIN_STEP_LIMIT), MAX_STEP_LIMIT));
  };

  return (
    <Box>
      <Typography
        variant="bodyMedium"
        component="p"
        sx={{ mb: 0.5 }}
      >
        {t('widgets.llmModelSelector.stepsLimitInput.label', 'Steps limit')}
      </Typography>
      <TextField
        type="number"
        value={value ?? DEFAULT_STEPS_LIMIT}
        onChange={handleChange}
        size="small"
        slotProps={{ htmlInput: { min: MIN_STEP_LIMIT, max: MAX_STEP_LIMIT } }}
        sx={{ width: '100%' }}
      />
    </Box>
  );
});

StepsLimitInput.displayName = 'StepsLimitInput';
