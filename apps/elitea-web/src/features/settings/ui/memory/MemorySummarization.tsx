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
import { SettingsToggleCard } from './SettingsToggleCard';

export const MemorySummarization = memo(() => {
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
          'settings.memory.summarization.description',
          'Summarize older messages to free up context space when the limit is reached',
        )}
        checked={values.enable_summarization}
        onToggle={handleSummarizationEnabledChange}
        disabled={!values.context_enabled}
        switchAriaLabel={t('settings.memory.summarization.enableAriaLabel', 'Enable automatic summarization')}
      />

      <Box sx={styles.section}>
        <InputBase
          data-testid="summary-instructions-input"
          label={t('settings.memory.summarization.instructions', 'Summarization instructions')}
          tooltipDescription={t(
            'settings.memory.summarization.instructionsTooltip',
            'Custom instructions for how summaries should be generated',
          )}
          autoComplete="off"
          outlined
          expand={{ minRows: 5, maxRows: 8 }}
          value={values.summary_llm_settings.instructions}
          onChange={handleInstructionsChange}
          error={Boolean(fieldErrors?.instructions)}
          helperText={fieldErrors?.instructions ?? ' '}
          disabled={isSummarizationDisabled}
          placeholder={CONTEXT_MESSAGES.DEFAULT_SUMMARY_INSTRUCTION}
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
