/**
 * What the graph holds, and the two tools that repair it.
 *
 * `—` and `0` are different facts. A count nobody reported is unknown; printing
 * zero for it reports an empty graph for a document the reader could not
 * parse — and the screen gives the user no way to tell those apart.
 *
 * The breakdowns are deliberately NOT truncated: a type that does not appear is
 * exactly the type a user opens this tab looking for, and "top ten" is
 * invisible on screen.
 *
 * Both maintenance buttons are disabled while either runs, because they rewrite
 * the same stored graph.
 */
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { InventoryStats } from '@/entities/inventory';

import { renderWithProviders } from '../__tests__/testUtils';
import { InventoryStatsPanel } from './InventoryStatsPanel';

const STATS: InventoryStats = {
  nodeCount: 1234,
  edgeCount: 5,
  byType: [
    { name: 'class', count: 3 },
    { name: 'document', count: 2 },
  ],
  byLayer: [{ name: 'application', count: 4 }],
  sourceToolkits: ['code', 'docs'],
};

const EMPTY_STATS: InventoryStats = {
  nodeCount: null,
  edgeCount: null,
  byType: [],
  byLayer: [],
  sourceToolkits: [],
};

function show(overrides: Partial<React.ComponentProps<typeof InventoryStatsPanel>> = {}) {
  const onRunMaintenance = vi.fn();
  renderWithProviders(
    <InventoryStatsPanel
      stats={STATS}
      cachedGraphs={2}
      cacheSizeBytes={4096}
      isPending={false}
      error={null}
      maintenanceRunning={null}
      maintenanceMessage={null}
      maintenanceError={null}
      onRunMaintenance={onRunMaintenance}
      {...overrides}
    />,
  );
  return onRunMaintenance;
}

describe('InventoryStatsPanel', () => {
  it('says it is reading rather than showing zeroes', () => {
    show({ isPending: true, stats: EMPTY_STATS });
    expect(screen.getByTestId('inventory-stats-pending')).toBeInTheDocument();
    expect(screen.queryByTestId('inventory-stats-panel')).not.toBeInTheDocument();
  });

  it('shows the provider own sentence instead of the counts when the read failed', () => {
    show({ error: 'No graph has been built for bucket graphs.' });
    expect(screen.getByTestId('inventory-stats-error')).toHaveTextContent(
      'No graph has been built for bucket graphs.',
    );
    expect(screen.queryByTestId('inventory-stat-entities')).not.toBeInTheDocument();
  });

  it('renders the four counters, grouped for reading', () => {
    show();
    expect(screen.getByTestId('inventory-stat-entities')).toHaveTextContent('1,234');
    expect(screen.getByTestId('inventory-stat-relations')).toHaveTextContent('5');
    expect(screen.getByTestId('inventory-stat-cached')).toHaveTextContent('2');
    expect(screen.getByTestId('inventory-stat-cache-bytes')).toHaveTextContent('4,096');
  });

  it('shows an unreported count as — and not as zero', () => {
    show({ stats: EMPTY_STATS, cachedGraphs: null, cacheSizeBytes: null });
    expect(screen.getByTestId('inventory-stat-entities')).toHaveTextContent('—');
    expect(screen.getByTestId('inventory-stat-cache-bytes')).toHaveTextContent('—');
  });

  it('renders EVERY entry of a breakdown', () => {
    show();
    const byType = screen.getByTestId('inventory-stats-by-type');
    expect(within(byType).getByText('class: 3')).toBeInTheDocument();
    expect(within(byType).getByText('document: 2')).toBeInTheDocument();
    expect(
      within(screen.getByTestId('inventory-stats-by-layer')).getByText('application: 4'),
    ).toBeInTheDocument();
  });

  it('says a breakdown is empty rather than rendering nothing', () => {
    show({ stats: EMPTY_STATS });
    expect(screen.getByTestId('inventory-stats-by-type')).toHaveTextContent(
      'Nothing to break down yet.',
    );
    expect(screen.getByTestId('inventory-stats-sources')).toHaveTextContent(
      'No source has contributed to this graph yet.',
    );
  });

  it('names each source that contributed to the graph', () => {
    show();
    const sources = screen.getByTestId('inventory-stats-sources');
    expect(within(sources).getByText('code')).toBeInTheDocument();
    expect(within(sources).getByText('docs')).toBeInTheDocument();
  });

  it('runs the maintenance tool the user chose', async () => {
    const user = userEvent.setup();
    const onRunMaintenance = show();
    await user.click(screen.getByTestId('inventory-normalize-types'));
    expect(onRunMaintenance).toHaveBeenCalledWith('normalize_types');
    await user.click(screen.getByTestId('inventory-rebuild-indices'));
    expect(onRunMaintenance).toHaveBeenLastCalledWith('rebuild_indices');
  });

  it('disables BOTH maintenance controls while either one runs', () => {
    show({ maintenanceRunning: 'normalize_types' });
    expect(screen.getByTestId('inventory-normalize-types')).toBeDisabled();
    expect(screen.getByTestId('inventory-rebuild-indices')).toBeDisabled();
  });

  it('shows what the maintenance tool answered', () => {
    show({ maintenanceMessage: 'Types are already normalised: 3 distinct types.' });
    expect(screen.getByTestId('inventory-maintenance-result')).toHaveTextContent(
      'Types are already normalised: 3 distinct types.',
    );
  });

  it('shows a refused maintenance run as an error', () => {
    show({ maintenanceError: 'Inventory could not run normalize_types.' });
    expect(screen.getByText('Inventory could not run normalize_types.')).toBeInTheDocument();
    expect(screen.queryByTestId('inventory-maintenance-result')).not.toBeInTheDocument();
  });
});
