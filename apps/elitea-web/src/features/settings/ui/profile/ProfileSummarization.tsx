/**
 * ProfileSummarization — summarization settings form.
 */
import { memo, useCallback, useMemo } from 'react';

import Box from '@mui/material/Box';

import { AccordionConstants } from '@/shared/lib/constants';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { useFormikContext } from 'formik';

import { createContextStrategyFormData, type ProfileFormValues } from '@/features/settings/lib/profile/profileUtils';
import ContextStrategySummarization from './context-budget/ContextStrategySummarization';
import { handleConvertToNumberChange } from '@/features/settings/lib/profile/context-budget/validation';
import { SummaryModelSelect } from '../memory/SummaryModelSelect';
import { t } from '@/shared/i18n';

export interface ProfileSummarizationProps {
  modelList: Array<{
    name: string;
    project_id: string;
    default?: boolean;
    display_name?: string;
  }>;
}

export const ProfileSummarization = memo(({ modelList }: ProfileSummarizationProps) => {
  const { values, errors, setFieldValue } = useFormikContext<ProfileFormValues>();


  /*
   * [#71] Was an inline object literal duplicating
   * `createContextStrategyFormData` field-for-field, which left the real helper
   * with no callers. Calling it is what the baseline does at the equivalent
   * spot (`pages/UserSettings/components/ProfileSummarization.jsx:22`,
   * `useMemo(() => createContextStrategyFormData(values), [values])`).
   */
  const contextFormData = useMemo(() => createContextStrategyFormData(values), [values]);

  const contextErrors = useMemo(
    () => ({
      ...(errors.summary_llm_settings && {
        summary_llm_settings: errors.summary_llm_settings,
      }),
    }),
    [errors],
  );

  const handleInputChange = useCallback(
    (
      _event: React.ChangeEvent<HTMLInputElement> | React.SyntheticEvent,
      field: string,
    ) => {
      const target = _event.target as HTMLInputElement;
      if (target.type === 'checkbox') {
        void setFieldValue(field, target.checked);
        return;
      }
      const setValue = (f: string, v: unknown) => void setFieldValue(f, v);
      handleConvertToNumberChange(target.value, field, setValue);
    },
    [setFieldValue],
  );

  const handleSummaryLLMInputChange = useCallback(
    (
      event: React.ChangeEvent<HTMLInputElement>,
      field: string,
      isNumeric?: boolean,
    ) => {
      if (isNumeric) {
        const setValue = (f: string, v: unknown) => void setFieldValue(f, v);
        handleConvertToNumberChange(event.target.value, `summary_llm_settings.${field}`, setValue);
        return;
      }
      void setFieldValue(`summary_llm_settings.${field}`, event.target.value);
    },
    [setFieldValue],
  );

  return (
    <BasicAccordion
      showMode={AccordionConstants.AccordionShowMode.LeftMode}
      defaultExpanded
      slotSx={{ accordion: { background: 'transparent' } }}
      items={[
        {
          title: t('settings.profile.summarization.title', 'Default Summarization'),
          content: (
            <Box sx={styles.accordionContent}>
              <SummaryModelSelect models={modelList} disabled={!values.context_enabled || !values.enable_summarization} />
              <ContextStrategySummarization
                formData={contextFormData}
                errors={contextErrors}
                handleInputChange={(e, f) => handleInputChange(e, f)}
                handleSummaryLLMInputChange={(e, f, n) => handleSummaryLLMInputChange(e, f, n)}
                isEnabled={values.context_enabled}
              />
            </Box>
          ),
        },
      ]}
    />
  );
});

ProfileSummarization.displayName = 'ProfileSummarization';

const styles = {
  accordionContent: {
    display: 'flex',
    flexDirection: 'column',
    gap: '1rem',
    paddingRight: '1rem',
    paddingTop: '0.6rem',
  },
};
