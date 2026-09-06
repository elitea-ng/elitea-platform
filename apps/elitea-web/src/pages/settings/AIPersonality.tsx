/**
 * AI Personality page (settings tab) — replaces the old app's
 * `features/settings/ui/ai-personality/AIPersonality.jsx`.
 *
 * A thin shell: a header row plus the feature body. The fetch, the Formik
 * host, the `PUT /social/author` save and its toasts all live in the
 * feature's `SettingsFormProvider`, which Settings › Memory shares — the two
 * pages edit two halves of one author record.
 */
import { memo } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { aiPersonalityFeature } from '@/features/settings';

const { AIPersonalityFormContent, SettingsFormProvider } = aiPersonalityFeature;

interface AIPersonalityProps {
  /** Currently-selected project id — threaded down from the route. */
  projectId?: string;
}

/* No header row — the route renders it. See `Preferences.tsx`. */
const AIPersonality = memo(({ projectId }: AIPersonalityProps) => (
  <Box sx={styles.content}>
    <SettingsFormProvider {...(projectId === undefined ? {} : { projectId })}>
      <AIPersonalityFormContent />
    </SettingsFormProvider>
  </Box>
));

AIPersonality.displayName = 'AIPersonality';

export default AIPersonality;

const styles: Record<string, SxProps<Theme>> = {
  content: (theme) => ({
    backgroundColor: theme.vars.palette.background.tabPanel,
    height: '100%',
    width: '100%',
    minHeight: 0,
    overflowY: 'auto',
  }),
};
