/**
 * MemoryContextManagement — the "Context Management" accordion, and the only
 * section of Settings › Memory.
 *
 * Baseline: `EliteaUI/src/[fsd]/features/settings/ui/memory/
 * MemoryContextManagement.jsx`. Order, top to bottom: the enable card, the
 * two numeric fields, the context-editing card, then the nested
 * summarization block — everything below the enable card is hidden while
 * context management is off.
 */
import { memo, useCallback } from 'react';

import Box from '@mui/material/Box';
import { useFormikContext } from 'formik';

import { handleConvertToNumberChange } from '@/features/settings/lib/profile/context-budget/validation';
import { t } from '@/shared/i18n';
import { AccordionConstants } from '@/shared/lib/constants';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { InfoLabelWithTooltip } from '@/shared/ui/InfoLabelWithTooltip';
import { ContextBudgetModeControl } from '@/shared/ui/ContextBudgetModeControl';
import { InputBase } from '@/shared/ui/InputBase';

import type { SettingsProfileFormValues } from '../ai-personality/settingsProfileForm';
import type { SummaryModelOption } from './SummaryModelSelect';
import { MemorySummarization } from './MemorySummarization';
import { SettingsToggleCard } from './SettingsToggleCard';

type NumericField = 'preserve_recent_messages';

export interface MemoryContextManagementProps {
  models?: readonly SummaryModelOption[];
  /** Called after a change that must persist immediately (both toggles). */
  onAutoSaveRequested?: () => void;
}

export const MemoryContextManagement = memo(({ onAutoSaveRequested, models = [] }: MemoryContextManagementProps) => {
  const { values, errors, setFieldValue } = useFormikContext<SettingsProfileFormValues>();

  const handleContextEnabledChange = useCallback(
    (checked: boolean) => {
      void setFieldValue('context_enabled', checked);
      onAutoSaveRequested?.();
    },
    [onAutoSaveRequested, setFieldValue],
  );

  const handleContextEditingChange = useCallback(
    (checked: boolean) => {
      void setFieldValue('enable_context_editing', checked);
      onAutoSaveRequested?.();
    },
    [onAutoSaveRequested, setFieldValue],
  );

  const handleNumericChange = useCallback(
    (value: string, field: NumericField) => {
      handleConvertToNumberChange(value, field, (name, next) => void setFieldValue(name, next));
    },
    [setFieldValue],
  );

  return (
    <BasicAccordion
      data-testid="context-management-section"
      showMode={AccordionConstants.AccordionShowMode.LeftMode}
      defaultExpanded
      slotSx={{ accordion: { background: 'transparent' }, details: { paddingTop: '0rem' } }}
      items={[
        {
          title: t('settings.memory.contextManagement.title', 'Context Management'),
          content: (
            <Box sx={styles.accordionContent}>
              <SettingsToggleCard
                data-testid="context-management-toggle"
                title={t('settings.memory.contextManagement.cardTitle', 'Context Management')}
                description={t(
                  'settings.memory.contextManagement.cardDescription',
                  'Enable context management for new conversations',
                )}
                checked={values.context_enabled}
                onToggle={handleContextEnabledChange}
                switchAriaLabel={t(
                  'settings.memory.contextManagement.enableAriaLabel',
                  'Enable context management',
                )}
              />

              {values.context_enabled && (
                <>
                  <ContextBudgetModeControl
                    value={values.budget_mode}
                    onChange={(mode) => { void setFieldValue('budget_mode', mode); onAutoSaveRequested?.(); }}
                  />
                  <Box sx={styles.fieldsRow}>
                    <Box sx={styles.field}>
                      <InfoLabelWithTooltip
                        label={t(
                          'settings.memory.contextManagement.preserveRecentMessages',
                          'Preserve Recent Messages',
                        )}
                        tooltip={t(
                          'settings.memory.contextManagement.preserveRecentMessagesTooltip',
                          'Preferred number of recent messages to keep verbatim. Large messages may be summarized to free context. The current request and active tool exchanges remain protected.',
                        )}
                      />
                      <InputBase
                        type="text"
                        inputMode="numeric"
                        value={values.preserve_recent_messages}
                        onChange={(event) =>
                          handleNumericChange(event.target.value, 'preserve_recent_messages')
                        }
                        error={Boolean(errors.preserve_recent_messages)}
                        helperText={errors.preserve_recent_messages ?? ' '}
                        slotProps={{
                          htmlInput: { pattern: '[1-9][0-9]*', 'data-testid': 'preserve-recent-messages-input' },
                        }}
                        containerSx={styles.inputContainer}
                      />
                    </Box>
                  </Box>

                  <SettingsToggleCard
                    data-testid="context-editing-toggle"
                    title={t('settings.memory.contextEditing.title', 'Context Editing')}
                    description={t(
                      'settings.memory.contextEditing.description',
                      'Clear older tool outputs when the context grows large, keeping recent results',
                    )}
                    checked={values.enable_context_editing}
                    onToggle={handleContextEditingChange}
                    switchAriaLabel={t('settings.memory.contextEditing.enableAriaLabel', 'Enable context editing')}
                  />

                  <Box sx={styles.subSections}>
                    <MemorySummarization models={models} />
                  </Box>
                </>
              )}
            </Box>
          ),
        },
      ]}
    />
  );
});

MemoryContextManagement.displayName = 'MemoryContextManagement';

const styles = {
  accordionContent: {
    display: 'flex',
    flexDirection: 'column',
    gap: '1rem',
    paddingRight: '1rem',
  },
  fieldsRow: {
    display: 'flex',
    gap: '1.5rem',
  },
  field: {
    display: 'flex',
    flexDirection: 'column',
    gap: '0.5rem',
    flex: 1,
  },
  subSections: {
    display: 'flex',
    flexDirection: 'column',
  },
  inputContainer: {
    padding: '0rem',
    margin: '0rem',
  },
};
