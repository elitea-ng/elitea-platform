/**
 * Preferences page (settings tab) — replaces the old app's
 * `features/settings/ui/preference/Preferences.jsx`.
 *
 * A thin page shell only: a header row plus the feature body. Every control
 * on this page persists itself (theme mode via `useColorScheme`, voice and
 * sound config via `localStorage`), so there is no fetch, no form and no
 * save bar here — unlike the sibling `Personalization.tsx`.
 */
import { memo } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { preferencesFeature } from '@/features/settings';

const { PreferencesFormContent } = preferencesFeature;

interface PreferencesProps {
  /** Currently-selected project id — threaded down from the route. */
  projectId: string;
}

/*
 * NO HEADER ROW HERE. `routes/_shell/settings/preferences.tsx` already
 * renders `DrawerPageHeader` above this component, so this page's own header
 * drew the word "Preferences" a second time, 60px below the first, with a
 * second hairline under it. Same defect on AI Personality, Memory and
 * Personalization.
 */
const Preferences = memo(({ projectId }: PreferencesProps) => (
  <Box sx={styles.content}>
    <PreferencesFormContent projectId={projectId} />
  </Box>
));

Preferences.displayName = 'Preferences';

export default Preferences;

const styles: Record<string, SxProps<Theme>> = {
  content: (theme) => ({
    // Baseline `preference/Preferences.jsx`'s `content`.
    backgroundColor: theme.vars.palette.background.tabPanel,
    height: '100%',
    width: '100%',
    minHeight: 0,
    overflowY: 'auto',
  }),
};
