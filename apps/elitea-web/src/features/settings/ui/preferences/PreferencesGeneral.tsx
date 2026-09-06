/**
 * PreferencesGeneral — the "General" accordion of Settings > Preferences.
 *
 * One row: a `Theme` label plus `shared/ui`'s `ThemeModeToggle`. No API call
 * — the toggle drives MUI's `useColorScheme`, which persists the mode itself
 * (`BrandThemeProvider`'s `modeStorageKey: 'el-mode'`).
 *
 * Mirrors the baseline's `features/settings/ui/preference/PreferenceGeneral.jsx`,
 * and is deliberately the same markup as the General block of the sibling
 * `profile/ProfilePersonalization.tsx` — the baseline shows this row on both
 * screens.
 */
import Box from '@mui/material/Box';

import { AccordionConstants } from '@/shared/lib/constants';
import { InfoLabelWithTooltip } from '@/shared/ui/InfoLabelWithTooltip';
import ThemeModeToggle from '@/shared/ui/ThemeModeToggle';
import { t } from '@/shared/i18n';

import { ProfileBasicAccordion } from '../profile/ProfileBasicAccordion';

export function PreferencesGeneral() {
  return (
    <ProfileBasicAccordion
      showMode={AccordionConstants.AccordionShowMode.LeftMode}
      defaultExpanded
      title={t('settings.general', 'General')}
      slotSx={{ accordion: { background: 'transparent' } }}
      data-testid="preferences-general-section"
      content={
        <Box sx={styles.content}>
          <Box sx={styles.row}>
            <InfoLabelWithTooltip
              label={t('settings.theme', 'Theme')}
              tooltip={t('settings.themeTooltip', 'Choose between light and dark theme')}
              sx={styles.label}
            />
            <Box sx={styles.toggle}>
              <ThemeModeToggle />
            </Box>
          </Box>
        </Box>
      }
    />
  );
}

/*
 * The 0.75rem indents are the baseline's (`preference/PreferenceGeneral.jsx`
 * `label` / `themeToggleContainer`) and are what puts the label and the
 * toggle 12px inside the accordion body — the same inset every other
 * settings control on this page sits at. Without them both sat flush against
 * the accordion's left edge.
 */
const styles = {
  content: {
    display: 'flex',
    flexDirection: 'column',
    gap: '1.25rem',
    paddingRight: '1rem',
    marginTop: '0.6rem',
  },
  row: {
    display: 'flex',
    flexDirection: 'column',
  },
  label: {
    paddingLeft: '0.75rem',
  },
  toggle: {
    display: 'flex',
    marginTop: '0.5rem',
    paddingLeft: '0.75rem',
  },
};
