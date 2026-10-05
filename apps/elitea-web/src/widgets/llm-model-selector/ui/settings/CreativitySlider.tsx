import { memo } from 'react';

import { Box, Slider, Typography } from '@mui/material';

import { DEFAULT_TEMPERATURE } from '@/shared/lib/constants';
import { t } from '@/shared/i18n';

const temperatureLabel = t('widgets.llmModelSelector.creativitySlider.label', 'Temperature');

interface CreativitySliderProps {
  temperature: number;
  onChange: (value: number) => void;
}

/** Temperature / creativity slider. */
export const CreativitySlider = memo(({ temperature, onChange }: CreativitySliderProps) => {
  const handleTemperatureChange = (_event: unknown, value: number | number[]) => {
    const num = (Array.isArray(value) ? value[0] : value) ?? DEFAULT_TEMPERATURE;
    onChange(num);
  };

  return (
    <Box>
      <Box sx={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', mb: 0.5 }}>
        <Typography variant="bodyMedium" component="p">{temperatureLabel}</Typography>
        <Typography variant="bodyMedium" component="p">{temperature?.toFixed(2)}</Typography>
      </Box>
      <Slider
        value={typeof temperature === 'number' ? temperature : DEFAULT_TEMPERATURE}
        onChange={handleTemperatureChange}
        min={0}
        max={2}
        step={0.01}
        aria-label={temperatureLabel}
      />
    </Box>
  );
});

CreativitySlider.displayName = 'CreativitySlider';
