/**
 * ProfilePersonalization — personality and instructions settings.
 */
import { useCallback, useMemo } from 'react';

import Box from '@mui/material/Box';

import { AccordionConstants } from '@/shared/lib/constants';
import { InfoLabelWithTooltip } from '@/shared/ui/InfoLabelWithTooltip';
import { InputBase } from '@/shared/ui/InputBase';
import { SingleSelect } from '@/shared/ui/SingleSelect';
import ThemeModeToggle from '@/shared/ui/ThemeModeToggle';
import { useFormikContext } from 'formik';

import { t } from '@/shared/i18n';

import type { ProfileFormValues } from '@/features/settings/lib/profile/profileUtils';
import { ProfileBasicAccordion } from './ProfileBasicAccordion';

export interface ProfilePersonalizationProps {
  onAutoSaveRequested?: () => void;
}

export function ProfilePersonalization({ onAutoSaveRequested }: ProfilePersonalizationProps) {
  const { values, setFieldValue } = useFormikContext<ProfileFormValues>();

  /*
   * Personality options — copied from old-app `common/constants.js`.
   *
   * The labels go through `t()` like every other user-visible string. The
   * former module-level array also carried a `description` for each option
   * that nothing ever rendered: the map below dropped it. Dead copy in
   * English is what this list used to be, so it is gone rather than
   * translated.
   */
  const personaOptions = useMemo(
    () => [
      { value: 'generic', label: t('settings.profile.persona.generic', 'Generic') },
      { value: 'qa', label: t('settings.profile.persona.qa', 'QA') },
      { value: 'nerdy', label: t('settings.profile.persona.nerdy', 'Nerdy') },
      { value: 'quirky', label: t('settings.profile.persona.quirky', 'Quirky') },
      { value: 'cynical', label: t('settings.profile.persona.cynical', 'Cynical') },
      { value: 'none', label: t('settings.profile.persona.none', 'None') },
      { value: 'bare', label: t('settings.profile.persona.bare', 'Bare') },
    ],
    [],
  );

  const handlePersonaChange = useCallback(
    (value: string) => {
      void setFieldValue('persona', value);
      onAutoSaveRequested?.();
    },
    [onAutoSaveRequested, setFieldValue],
  );

  const handleInstructionsChange = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => void setFieldValue('default_instructions', e.target.value),
    [setFieldValue],
  );

  return (
    <>
      <ProfileBasicAccordion
        showMode={AccordionConstants.AccordionShowMode.LeftMode}
        defaultExpanded
        title={t('settings.general', 'General')}
        slotSx={{ accordion: { background: 'transparent' } }}
        content={
          <Box sx={styles.accordionContent}>
            <Box sx={styles.section}>
              <InfoLabelWithTooltip
                label={t('settings.theme', 'Theme')}
                tooltip={t('settings.themeTooltip', 'Choose between light and dark theme')}
              />
              <Box sx={styles.themeToggleContainer}>
                <ThemeModeToggle />
              </Box>
            </Box>
          </Box>
        }
      />
      <ProfileBasicAccordion
        showMode={AccordionConstants.AccordionShowMode.LeftMode}
        defaultExpanded
        title={t('settings.personality', 'Default Personality Management')}
        slotSx={{ accordion: { background: 'transparent' } }}
        content={
          <Box sx={styles.accordionContent}>
            <Box sx={styles.section}>
              <InfoLabelWithTooltip
                variant="labelMedium"
                label={t('settings.defaultPersonality', 'Default Personality')}
                tooltip={t('settings.selectDefaultPersonality', 'Select the default assistant personality for your conversations')}
              />
              <SingleSelect
                value={values.persona}
                onChange={handlePersonaChange}
                placeholder=""
                options={personaOptions}
                sx={styles.inputSelect}
              />
            </Box>
            <Box sx={styles.section}>
              <InfoLabelWithTooltip
                label={t('settings.defaultUserInstructions', 'Default User Instructions')}
                tooltip={t('settings.customInstructions', 'Custom instructions that will be applied to all new conversations')}
              />
              <InputBase
                expand={{ minRows: 3, maxRows: 6 }}
                value={values.default_instructions}
                onChange={handleInstructionsChange}
                placeholder={t('settings.instructionsExample', 'Example: Always respond in a concise manner.')}
                containerSx={styles.inputContainer}
              />
            </Box>
          </Box>
        }
      />
    </>
  );
}

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
  themeToggleContainer: {
    marginTop: '0.5rem',
    paddingLeft: '0.75rem',
  },
  inputSelect: {
    marginTop: '0.25rem',
  },
  inputContainer: {
    padding: '0rem',
    margin: '0rem',
  },
};
