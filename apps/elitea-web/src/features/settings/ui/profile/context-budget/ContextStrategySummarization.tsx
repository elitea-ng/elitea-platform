/**
 * ContextStrategySummarization — presentational component for summarization.
 */
import { memo } from 'react';

import Box from '@mui/material/Box';

import { t } from '@/shared/i18n';
import { BaseSwitch } from '@/shared/ui/BaseSwitch';
import { InfoLabelWithTooltip } from '@/shared/ui/InfoLabelWithTooltip';
import { InputBase } from '@/shared/ui/InputBase';

import { CONTEXT_MESSAGES } from '@/features/settings/lib/profile/context-budget/constants';

interface SummaryLlmSettings {
  instructions: string;
  model_name: string;
  model_project_id: string | null;
  max_tokens: number;
}

interface ContextFormData {
  enabled: boolean;

  preserve_recent_messages: number;
  enable_summarization: boolean;
  summary_llm_settings: SummaryLlmSettings;
}

interface ContextStrategySummarizationErrors {
  summary_llm_settings?: Record<string, string> | undefined;
}

interface ContextStrategySummarizationProps {
  formData: ContextFormData;
  errors: ContextStrategySummarizationErrors;
  handleInputChange: (
    event: React.ChangeEvent<HTMLInputElement> | React.SyntheticEvent,
    field: string,
  ) => void;
  handleSummaryLLMInputChange: (
    event: React.ChangeEvent<HTMLInputElement>,
    field: string,
    isNumeric?: boolean,
  ) => void;
  isEnabled?: boolean;
}

const ContextStrategySummarization = memo(
  ({
    formData,
    errors,
    handleInputChange,
    handleSummaryLLMInputChange,
    isEnabled = true,
  }: ContextStrategySummarizationProps) => {
    return (
      <Box sx={styles.container}>
        {/* Enable Toggle */}
        <Box sx={styles.toggleSection}>
          <BaseSwitch
            checked={formData.enable_summarization}
            onChange={(_event, checkedValue) =>
              handleInputChange(
                { target: { checked: checkedValue, type: 'checkbox' } } as unknown as React.ChangeEvent<HTMLInputElement>,
                'enable_summarization',
              )
            }
            disabled={!isEnabled}
            slotProps={{
              input: {
                'aria-label': 'Enable summarization',
              },
            }}
          />
        </Box>

        {/* Summarization Instructions */}
        <Box sx={[styles.section, styles.sectionSummarizationInstruction]}>
          <InfoLabelWithTooltip
            label={t('settings.profile.contextBudget.summarization.instructions', 'Summarization instructions')}
            tooltip={t('settings.profile.contextBudget.summarization.instructionsTooltip', 'Custom instructions for how summaries should be generated')}
          />
          <InputBase
            expand={{ minRows: 3, maxRows: 6 }}
            value={formData.summary_llm_settings.instructions}
            onChange={(e) => handleSummaryLLMInputChange(e as React.ChangeEvent<HTMLInputElement>, 'instructions')}
            error={!!errors.summary_llm_settings?.instructions}
            helperText={errors.summary_llm_settings?.instructions}
            disabled={!isEnabled || !formData.enable_summarization}
            placeholder={CONTEXT_MESSAGES.DEFAULT_SUMMARY_INSTRUCTION}
            containerSx={styles.formInput}
          />
        </Box>

        <Box sx={styles.grid}>
          {/* Target Summary Tokens */}
          <Box sx={styles.section}>
            <InfoLabelWithTooltip
              label={t('settings.profile.contextBudget.summarization.targetTokens', 'Target Summary Tokens')}
              tooltip={t('settings.profile.contextBudget.summarization.targetTokensTooltip', 'Target length for summary generation')}
            />
            <InputBase
              type="text"
              inputMode="numeric"
              value={formData.summary_llm_settings.max_tokens}
              onChange={(e) =>
                handleSummaryLLMInputChange(e as React.ChangeEvent<HTMLInputElement>, 'max_tokens', true)
              }
              error={!!errors.summary_llm_settings?.max_tokens}
              helperText={errors.summary_llm_settings?.max_tokens}
              disabled={!isEnabled || !formData.enable_summarization}
              slotProps={{
                htmlInput: {
                  pattern: '[1-9][0-9]*',
                },
              }}
              containerSx={styles.formInput}
            />
          </Box>
        </Box>
      </Box>
    );
  },
);

ContextStrategySummarization.displayName = 'ContextStrategySummarization';

export default ContextStrategySummarization;

const styles = {
  container: {
    display: 'flex',
    flexDirection: 'column',
    paddingRight: '1rem',
  },
  toggleSection: {
    display: 'flex',
    alignItems: 'center',
    paddingLeft: '0.5rem',
  },
  section: {
    display: 'flex',
    flexDirection: 'column',
    gap: '0.5rem',
  },
  sectionSummarizationInstruction: {
    gap: 0,
    marginBottom: '1.15rem',
  },
  formInput: {
    padding: '0rem',
    margin: '0rem',
  },
  grid: {
    display: 'grid',
    gridTemplateColumns: 'repeat(2, 1fr)',
    gap: '0rem 1rem',
  },
};
