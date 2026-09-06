/**
 * AIPersonalityPersonalization — the "Persona Management" accordion.
 *
 * Baseline: `EliteaUI/src/[fsd]/features/settings/ui/ai-personality/
 * AIPersonalityPersonalization.jsx`.
 *
 * Two behaviours that are easy to get wrong and are covered by this
 * component's tests:
 *  - instructions are stored PER PERSONA under
 *    `personality_instructions.<persona>`, so switching persona swaps which
 *    slot the one text field edits (and never carries text across);
 *  - the `none` persona applies no personality overlay, so it has no
 *    instructions field at all.
 */
import { memo, useCallback, useMemo } from 'react';

import Box from '@mui/material/Box';
import { useFormikContext } from 'formik';

import { t } from '@/shared/i18n';
import { AccordionConstants } from '@/shared/lib/constants';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { InfoLabelWithTooltip } from '@/shared/ui/InfoLabelWithTooltip';
import { InputBase } from '@/shared/ui/InputBase';
import { SingleSelect } from '@/shared/ui/SingleSelect';

import {
  NONE_PERSONA,
  buildPersonaOptions,
  personaInstructionsPlaceholder,
} from './personaOptions';
import type { SettingsProfileFormValues } from './settingsProfileForm';

export interface AIPersonalityPersonalizationProps {
  /** Called after a change that must persist immediately (the persona select). */
  onAutoSaveRequested?: () => void;
}

export const AIPersonalityPersonalization = memo(
  ({ onAutoSaveRequested }: AIPersonalityPersonalizationProps) => {
    const { values, setFieldValue } = useFormikContext<SettingsProfileFormValues>();
    const personaOptions = useMemo(() => buildPersonaOptions(), []);
    const { persona } = values;

    const handlePersonaChange = useCallback(
      (value: string) => {
        void setFieldValue('persona', value);
        onAutoSaveRequested?.();
      },
      [onAutoSaveRequested, setFieldValue],
    );

    const handleInstructionsChange = useCallback(
      (event: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
        void setFieldValue(`personality_instructions.${persona}`, event.target.value);
      },
      [persona, setFieldValue],
    );

    return (
      <BasicAccordion
        data-testid="persona-management-section"
        showMode={AccordionConstants.AccordionShowMode.LeftMode}
        defaultExpanded
        slotSx={{ accordion: { background: 'transparent' } }}
        items={[
          {
            title: t('settings.aiPersonality.personaManagement.title', 'Persona Management'),
            content: (
              <Box sx={styles.accordionContent}>
                <Box sx={styles.section}>
                  <InfoLabelWithTooltip
                    label={t('settings.aiPersonality.defaultPersona', 'Default persona')}
                    tooltip={t(
                      'settings.aiPersonality.defaultPersonaTooltip',
                      'Select the default assistant persona for your conversations',
                    )}
                  />
                  <SingleSelect
                    value={persona}
                    onChange={handlePersonaChange}
                    options={personaOptions}
                    placeholder=""
                    sx={styles.inputSelect}
                  />
                </Box>

                {persona !== NONE_PERSONA && (
                  <Box sx={styles.section}>
                    <InputBase
                      data-testid="persona-instructions-input"
                      label={t('settings.aiPersonality.userInstructions', 'User instructions')}
                      tooltipDescription={t(
                        'settings.aiPersonality.userInstructionsTooltip',
                        'Custom instructions for the selected persona, applied to new conversations that use it. Each persona keeps its own instructions.',
                      )}
                      autoComplete="off"
                      outlined
                      expand={{ minRows: 5, maxRows: 8 }}
                      value={values.personality_instructions[persona] ?? ''}
                      onChange={handleInstructionsChange}
                      placeholder={personaInstructionsPlaceholder(persona)}
                      actions={{ enabled: true, showCopy: true }}
                      containerSx={styles.inputContainer}
                    />
                  </Box>
                )}
              </Box>
            ),
          },
        ]}
      />
    );
  },
);

AIPersonalityPersonalization.displayName = 'AIPersonalityPersonalization';

const styles = {
  accordionContent: {
    display: 'flex',
    flexDirection: 'column',
    gap: '1.25rem',
    paddingRight: '1rem',
    marginTop: '0.6rem',
  },
  section: {
    display: 'flex',
    flexDirection: 'column',
  },
  inputSelect: {
    marginTop: '0.25rem',
  },
  inputContainer: {
    padding: '0rem',
    margin: '0rem',
  },
};
