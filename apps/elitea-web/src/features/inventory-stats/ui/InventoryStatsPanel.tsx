/**
 * What the graph holds, and the two tools that repair it.
 *
 * THE BREAKDOWNS ARE NOT TRUNCATED. The legacy panel showed the top ten types
 * and nothing else, which is defensible on a 300-pixel rail and not here: a
 * type that does not appear is exactly the type a user is looking for when
 * they open this tab, and "top ten" is invisible on screen.
 */
import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';

import type { InventoryCount, InventoryStats } from '@/entities/inventory';
import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import type { MaintenanceTool } from '../model/useInventoryStats';

const sectionSx = { display: 'flex', flexDirection: 'column', gap: 2 } as const;
const chipRowSx = { display: 'flex', flexWrap: 'wrap', gap: 0.5 } as const;
const countsRowSx = { display: 'flex', flexWrap: 'wrap', gap: 3 } as const;

export interface InventoryStatsPanelProps {
  readonly stats: InventoryStats;
  readonly cachedGraphs: number | null;
  readonly cacheSizeBytes: number | null;
  readonly isPending: boolean;
  readonly error: string | null;
  readonly maintenanceRunning: MaintenanceTool | null;
  readonly maintenanceMessage: string | null;
  readonly maintenanceError: string | null;
  readonly onRunMaintenance: (tool: MaintenanceTool) => void;
}

/** `—` for a count nobody reported, which is not the same fact as zero. */
function countText(value: number | null): string {
  return value === null ? '—' : value.toLocaleString();
}

export function InventoryStatsPanel({
  stats,
  cachedGraphs,
  cacheSizeBytes,
  isPending,
  error,
  maintenanceRunning,
  maintenanceMessage,
  maintenanceError,
  onRunMaintenance,
}: InventoryStatsPanelProps): React.JSX.Element {
  if (isPending) {
    return (
      <Typography variant="bodyMedium" data-testid="inventory-stats-pending">
        {t('inventory.stats.loading', 'Reading the statistics…')}
      </Typography>
    );
  }

  if (error !== null) {
    return (
      <Box data-testid="inventory-stats-error">
        <BannerMessage variant="error" message={error} />
      </Box>
    );
  }

  return (
    <Box sx={sectionSx} data-testid="inventory-stats-panel">
      <Box sx={countsRowSx}>
        <Counter
          label={t('inventory.stats.entities', 'Entities')}
          value={countText(stats.nodeCount)}
          testId="inventory-stat-entities"
        />
        <Counter
          label={t('inventory.stats.relations', 'Relations')}
          value={countText(stats.edgeCount)}
          testId="inventory-stat-relations"
        />
        <Counter
          label={t('inventory.stats.cachedGraphs', 'Cached graphs')}
          value={countText(cachedGraphs)}
          testId="inventory-stat-cached"
        />
        <Counter
          label={t('inventory.stats.cacheBytes', 'Cache size (bytes)')}
          value={countText(cacheSizeBytes)}
          testId="inventory-stat-cache-bytes"
        />
      </Box>

      <Breakdown
        title={t('inventory.stats.byType', 'Entity types')}
        counts={stats.byType}
        testId="inventory-stats-by-type"
      />
      <Breakdown
        title={t('inventory.stats.byLayer', 'Layers')}
        counts={stats.byLayer}
        testId="inventory-stats-by-layer"
      />

      <Box>
        <Typography variant="labelSmall" color="text.secondary">
          {t('inventory.stats.sources', 'Sources')}
        </Typography>
        <Box sx={chipRowSx} data-testid="inventory-stats-sources">
          {stats.sourceToolkits.length === 0 ? (
            <Typography variant="bodySmall" color="text.secondary">
              {t('inventory.stats.noSources', 'No source has contributed to this graph yet.')}
            </Typography>
          ) : (
            stats.sourceToolkits.map((source) => <Chip key={source} size="small" label={source} />)
          )}
        </Box>
      </Box>

      <Box sx={sectionSx}>
        <Typography variant="headingSmall">{t('inventory.stats.maintenance', 'Maintenance')}</Typography>
        <Typography variant="bodySmall" color="text.secondary">
          {t(
            'inventory.stats.maintenanceHelp',
            'Normalising merges type variations such as Feature and feature. Rebuilding repairs the name, type, file and source indices.',
          )}
        </Typography>
        <Stack direction="row" spacing={1}>
          <BaseBtn
            variant="secondary"
            size="small"
            data-testid="inventory-normalize-types"
            disabled={maintenanceRunning !== null}
            onClick={() => {
              onRunMaintenance('normalize_types');
            }}
          >
            {t('inventory.stats.normalize', 'Normalise types')}
          </BaseBtn>
          <BaseBtn
            variant="secondary"
            size="small"
            data-testid="inventory-rebuild-indices"
            disabled={maintenanceRunning !== null}
            onClick={() => {
              onRunMaintenance('rebuild_indices');
            }}
          >
            {t('inventory.stats.rebuild', 'Rebuild indices')}
          </BaseBtn>
        </Stack>
        {maintenanceMessage === null ? null : (
          <Typography variant="bodySmall" data-testid="inventory-maintenance-result">
            {maintenanceMessage}
          </Typography>
        )}
        {maintenanceError === null ? null : (
          <BannerMessage variant="error" message={maintenanceError} />
        )}
      </Box>
    </Box>
  );
}

interface CounterProps {
  readonly label: string;
  readonly value: string;
  readonly testId: string;
}

function Counter({ label, value, testId }: CounterProps): React.JSX.Element {
  return (
    <Box>
      <Typography variant="labelSmall" color="text.secondary">
        {label}
      </Typography>
      <Typography variant="headingMedium" data-testid={testId}>
        {value}
      </Typography>
    </Box>
  );
}

interface BreakdownProps {
  readonly title: string;
  readonly counts: readonly InventoryCount[];
  readonly testId: string;
}

function Breakdown({ title, counts, testId }: BreakdownProps): React.JSX.Element {
  return (
    <Box>
      <Typography variant="labelSmall" color="text.secondary">
        {title}
      </Typography>
      <Box sx={chipRowSx} data-testid={testId}>
        {counts.length === 0 ? (
          <Typography variant="bodySmall" color="text.secondary">
            {t('inventory.stats.emptyBreakdown', 'Nothing to break down yet.')}
          </Typography>
        ) : (
          counts.map((entry) => (
            <Chip key={entry.name} size="small" label={`${entry.name}: ${entry.count}`} />
          ))
        )}
      </Box>
    </Box>
  );
}
