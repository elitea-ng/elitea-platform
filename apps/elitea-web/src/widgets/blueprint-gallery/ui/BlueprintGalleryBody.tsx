/**
 * The gallery's body (issue 3137): Agents | Teams, then the blueprints of the
 * chosen kind as cards in the Agents grid's tracks.
 */
import type { ReactNode, SyntheticEvent } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { BaseTab } from '@/shared/ui/BaseTab';
import { BaseTabs } from '@/shared/ui/BaseTabs';

import { useBlueprintIndex, type BlueprintEntry } from '../model/blueprintIndex';
import { BlueprintCard } from './BlueprintCard';

export type GalleryTab = 'agents' | 'teams';

export interface BlueprintGalleryBodyProps {
  /** Fetches the index only while the gallery is open. */
  readonly enabled: boolean;
  readonly tab: GalleryTab;
  readonly onTabChange: (event: SyntheticEvent, value: GalleryTab) => void;
  readonly loadingSlug: string | undefined;
  readonly importError: string | undefined;
  readonly onImport: (entry: BlueprintEntry) => void;
  readonly onCraft: ((entry: BlueprintEntry) => void) | undefined;
}

function Cards({ entries, loadingSlug, onImport, onCraft }: Pick<BlueprintGalleryBodyProps, 'loadingSlug' | 'onImport' | 'onCraft'> & { readonly entries: readonly BlueprintEntry[] }): ReactNode {
  if (entries.length === 0) {
    return (
      <Typography variant="bodyMedium" data-testid="blueprint-gallery-empty">
        {t('widgets.blueprintGallery.empty', 'This build ships no blueprints of this kind.')}
      </Typography>
    );
  }
  return (
    <Box sx={gridSx}>
      {entries.map((entry) => (
        <BlueprintCard key={entry.slug} entry={entry} importing={loadingSlug !== undefined} onImport={onImport} onCraft={onCraft} />
      ))}
    </Box>
  );
}

export function BlueprintGalleryBody({ enabled, tab, onTabChange, loadingSlug, importError, onImport, onCraft }: BlueprintGalleryBodyProps): ReactNode {
  const index = useBlueprintIndex(enabled);
  return (
    <Box sx={bodySx}>
      <Typography variant="bodyMedium" color="text.secondary">
        {t(
          'widgets.blueprintGallery.intro',
          'Ready-made agents and teams that show what the platform can do. Import one as it is, or craft your own from its brief with Build with AI.',
        )}
      </Typography>
      <BaseTabs value={tab} onChange={onTabChange} aria-label={t('widgets.blueprintGallery.tabs', 'Blueprint kinds')}>
        <BaseTab value="agents" label={t('widgets.blueprintGallery.agents', 'Agents')} data-testid="blueprint-tab-agents" />
        <BaseTab value="teams" label={t('widgets.blueprintGallery.teams', 'Teams')} data-testid="blueprint-tab-teams" />
      </BaseTabs>
      {importError !== undefined && (
        <Alert severity="error" data-testid="blueprint-import-error">
          {importError}
        </Alert>
      )}
      {index.isPending && enabled && <CircularProgress size="1.5rem" aria-label={t('widgets.blueprintGallery.loading', 'Loading blueprints')} />}
      {index.isError && (
        <Alert severity="error" data-testid="blueprint-gallery-error">
          {t('widgets.blueprintGallery.loadError', 'The blueprints could not be loaded.')}
        </Alert>
      )}
      {index.data !== undefined && (
        <Cards entries={tab === 'agents' ? index.data.agents : index.data.teams} loadingSlug={loadingSlug} onImport={onImport} onCraft={onCraft} />
      )}
    </Box>
  );
}

const bodySx: SxProps<Theme> = (theme: Theme) => ({ display: 'flex', flexDirection: 'column', gap: theme.spacing(2), minWidth: 0 });

/** The Agents grid's tracks (`EntityCardList`'s rich variant), as the Teams grid uses them. */
const gridSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'grid',
  gridTemplateColumns: 'repeat(auto-fill, minmax(min(100%, 320px), 1fr))',
  gridAutoRows: '1fr',
  gap: theme.spacing(2),
  width: '100%',
});
