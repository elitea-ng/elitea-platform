/**
 * Memory page (settings tab) — replaces the old app's
 * `features/settings/ui/memory/Memory.jsx`.
 *
 * A thin shell: a header row plus the feature body. Shares one save
 * mechanism with Settings › AI Personality (`SettingsFormProvider`) because
 * both pages edit the same author record and save it with the same
 * `PUT /social/author`.
 */
import { memo } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import { aiPersonalityFeature, memoryFeature } from '@/features/settings';

const { SettingsFormProvider } = aiPersonalityFeature;
const { MemoryFormContent } = memoryFeature;

interface MemoryProps {
  /** Currently-selected project id — threaded down from the route. */
  projectId?: string;
}

/* No header row — the route renders it. See `Preferences.tsx`. */
const Memory = memo(({ projectId }: MemoryProps) => (
  <Box sx={styles.content}>
    <SettingsFormProvider {...(projectId === undefined ? {} : { projectId })}>
      <MemoryFormContent />
    </SettingsFormProvider>
  </Box>
));

Memory.displayName = 'Memory';

export default Memory;

const styles: Record<string, SxProps<Theme>> = {
  content: (theme) => ({
    backgroundColor: theme.vars.palette.background.tabPanel,
    height: '100%',
    width: '100%',
    minHeight: 0,
    overflowY: 'auto',
  }),
};
