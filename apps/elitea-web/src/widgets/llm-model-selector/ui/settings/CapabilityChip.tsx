import { memo } from 'react';

import { Chip, Tooltip } from '@mui/material';

import { t } from '@/shared/i18n';

interface CapabilityChipProps {
  type: 'vision' | 'reasoning';
  showTooltip?: boolean;
}

const VISION_LABEL = t('widgets.llmModelSelector.capabilityChip.visionLabel', 'Vision');
const REASONING_LABEL = t('widgets.llmModelSelector.capabilityChip.reasoningLabel', 'Reasoning');
const VISION_TOOLTIP = t(
  'widgets.llmModelSelector.capabilityChip.visionTooltip',
  'Model supports image input',
);
const REASONING_TOOLTIP = t(
  'widgets.llmModelSelector.capabilityChip.reasoningTooltip',
  'Model supports reasoning',
);

/** Small chip showing model capability (vision/reasoning). */
export const CapabilityChip = memo(({ type, showTooltip = false }: CapabilityChipProps) => {
  const label = type === 'vision' ? VISION_LABEL : REASONING_LABEL;
  const tooltip = type === 'vision' ? VISION_TOOLTIP : REASONING_TOOLTIP;

  const chip = (
    <Chip
      label={label}
      size="small"
      sx={(theme) => ({
        height: '1.25rem',
        bgcolor:
          type === 'vision'
            ? theme.vars.palette.capability.vision.background
            : theme.vars.palette.capability.reasoning.background,
        color: theme.vars.palette.text.secondary,
        fontWeight: 500,
      })}
    />
  );

  return showTooltip ? <Tooltip title={tooltip}>{chip}</Tooltip> : chip;
});

CapabilityChip.displayName = 'CapabilityChip';
