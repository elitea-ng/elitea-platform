/**
 * MemorySummarization — the nested summarization block inside Settings ›
 * Memory's Context Management accordion.
 *
 * Baseline: `EliteaUI/src/[fsd]/features/settings/ui/memory/
 * MemorySummarization.jsx`.
 *
 * The disable gating is the non-obvious part and is covered by this
 * component's tests: the summarization SWITCH follows `context_enabled`
 * alone, while the two fields below it are disabled when EITHER
 * `context_enabled` or `enable_summarization` is off.
 */
import { memo, useCallback } from 'react';

import Box from '@mui/material/Box';
import { useFormikContext } from 'formik';

import { CONTEXT_MESSAGES } from '@/features/settings/lib/profile/context-budget/constants';
import { handleConvertToNumberChange } from '@/features/settings/lib/profile/context-budget/validation';
import { t } from '@/shared/i18n';
import { InputBase } from '@/shared/ui/InputBase';

import type { SettingsProfileFormValues } from '../ai-personality/settingsProfileForm';
import { SummaryModelSelect, type SummaryModelOption } from './SummaryModelSelect';
import { SettingsToggleCard } from './SettingsToggleCard';

export const MemorySummarization = memo(({ models = [] }: { models?: readonly SummaryModelOption[] }) => {
  const { values, errors, setFieldValue } = useFormikContext<SettingsProfileFormValues>();

  const isSummarizationDisabled = !values.context_enabled || !values.enable_summarization;

  const handleSummarizationEnabledChange = useCallback(
    (checked: boolean) => {
      void setFieldValue('enable_summarization', checked);
    },
    [setFieldValue],
  );

  const handleInstructionsChange = useCallback(
    (event: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      void setFieldValue('summary_llm_settings.instructions', event.target.value);
    },
    [setFieldValue],
  );

  const handleMaxTokensChange = useCallback(
    (event: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      handleConvertToNumberChange(
        event.target.value,
        'summary_llm_settings.max_tokens',
        (field, value) => void setFieldValue(field, value),
      );
    },
    [setFieldValue],
  );

  const fieldErrors = errors.summary_llm_settings;

  return (
    <Box sx={styles.container}>
      <SettingsToggleCard
        data-testid="automatic-summarization-toggle"
        title={t('settings.memory.summarization.title', 'Automatic Summarization')}
        description={t(
          'settings.memory.summarization.compactionDescription',
          'Compact older history at 90% of usable input, retaining recent messages and authoritative instructions.',
        )}
        checked={values.enable_summarization}
        onToggle={handleSummarizationEnabledChange}
        disabled={!values.context_enabled}
        switchAriaLabel={t('settings.memory.summarization.enableAriaLabel', 'Enable automatic summarization')}
      />

      <SummaryModelSelect models={models} disabled={isSummarizationDisabled} />

      <Box sx={styles.section}>
        <InputBase
          data-testid="summary-instructions-input"
          label={t('settings.memory.summarization.additionalGuidance', 'Additional summary guidance (optional)')}
          tooltipDescription={t(
            'settings.memory.summarization.contractDescription',
            CONTEXT_MESSAGES.SUMMARY_CONTRACT_DESCRIPTION,
          )}
          autoComplete="off"
          outlined
          expand={{ minRows: 5, maxRows: 8 }}
          value={values.summary_llm_settings.instructions}
          onChange={handleInstructionsChange}
          error={Boolean(fieldErrors?.instructions)}
          helperText={fieldErrors?.instructions ?? t('settings.memory.summarization.contractDescription', CONTEXT_MESSAGES.SUMMARY_CONTRACT_DESCRIPTION)}
          disabled={isSummarizationDisabled}
          placeholder={t('settings.memory.summarization.guidancePlaceholder', CONTEXT_MESSAGES.SUMMARY_GUIDANCE_PLACEHOLDER)}
          actions={{ enabled: true, showCopy: true }}
          containerSx={styles.inputContainer}
        />
      </Box>

      <Box sx={styles.halfWidthSection}>
        <InputBase
          data-testid="target-summary-tokens-input"
          label={t('settings.memory.summarization.targetTokens', 'Target Summary Tokens')}
          tooltipDescription={t(
            'settings.memory.summarization.targetTokensTooltip',
            'Target length for summary generation',
          )}
          type="text"
          inputMode="numeric"
          value={values.summary_llm_settings.max_tokens}
          onChange={handleMaxTokensChange}
          error={Boolean(fieldErrors?.max_tokens)}
          helperText={fieldErrors?.max_tokens ?? ' '}
          disabled={isSummarizationDisabled}
          containerSx={styles.inputContainer}
          slotProps={{ htmlInput: { pattern: '[1-9][0-9]*' } }}
        />
      </Box>
    </Box>
  );
});

MemorySummarization.displayName = 'MemorySummarization';

const styles = {
  container: {
    display: 'flex',
    flexDirection: 'column',
    gap: '1rem',
  },
  section: {
    display: 'flex',
    flexDirection: 'column',
    width: '100%',
  },
  /** Baseline `MemorySummarization.jsx`'s `halfWidthSection`. */
  halfWidthSection: {
    display: 'flex',
    flexDirection: 'column',
    width: '48%',
  },
  inputContainer: {
    padding: '0rem',
    margin: '0rem',
  },
};
