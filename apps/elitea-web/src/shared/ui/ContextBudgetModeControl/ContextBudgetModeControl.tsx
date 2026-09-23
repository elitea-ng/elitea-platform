import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { resolveContextBudgetMode, type ContextBudgetMode } from '@/shared/lib/contextBudget';
import { RadioButtonGroup } from '@/shared/ui/RadioButtonGroup';

export function ContextBudgetModeControl({ value, onChange, disabled = false }: {
  readonly value: ContextBudgetMode;
  readonly onChange: (value: ContextBudgetMode) => void;
  readonly disabled?: boolean;
}) {
  return (
    <Box data-testid="context-budget-mode-control" sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      <Typography variant="bodySmall">{t('contextBudget.mode.title', 'Context window')}</Typography>
      <RadioButtonGroup
        aria-label={t('contextBudget.mode.title', 'Context window')}
        value={value}
        disabled={disabled}
        onChange={(next) => onChange(resolveContextBudgetMode(next))}
        items={[
          { value: 'balanced', label: t('contextBudget.mode.balanced', 'Balanced'), description: t('contextBudget.mode.balancedHelp', 'Up to 272,000 tokens, within the model’s limits.') },
          { value: 'full', label: t('contextBudget.mode.full', 'Full'), description: t('contextBudget.mode.fullHelp', 'Use the model’s full context window. Larger requests may cost more.') },
        ]}
      />
      <Typography variant="bodySmall2" color="text.secondary">
        {t('contextBudget.mode.reserveHelp', 'Output and a safety margin are reserved inside the window. When automatic summarization is enabled, compaction starts at 90% of usable input.')}
      </Typography>
    </Box>
  );
}
