/**
 * What an ingestion the user started is doing.
 *
 * IT MUST BE ABSENT WHEN NOTHING HAS RUN. A banner that is always mounted
 * makes the Sources tab look as though an ingestion is under way on every
 * visit, and the control that stops one appears beside it.
 *
 * THE ARTIFACTS ARE THE POINT OF KEEPING IT AFTER THE RUN. The ingestion
 * checkpoint is named in the terminal body and nowhere else a screen can
 * reach; dropping the banner on completion would take the only record of it
 * with it.
 */
import { describe, expect, it, vi } from 'vitest';
import { screen } from '@testing-library/react';

import type { IngestionRun } from '@/features/inventory-sources';

import { renderWithProviders } from '../__tests__/testUtils';
import { IngestionBanner } from './IngestionBanner';

function run(overrides: Partial<IngestionRun> = {}): IngestionRun {
  return {
    runningSourceId: null,
    steps: [],
    error: null,
    summary: null,
    artifacts: [],
    start: vi.fn(),
    stop: vi.fn(),
    ...overrides,
  };
}

describe('IngestionBanner', () => {
  it('renders nothing before anything has run', () => {
    const { container } = renderWithProviders(<IngestionBanner run={run()} />);
    expect(container).toBeEmptyDOMElement();
  });

  it('shows the progress the provider streamed while a run is under way', () => {
    renderWithProviders(
      <IngestionBanner
        run={run({ runningSourceId: '9110', steps: ['Reading the source files', 'Extracting entities'] })}
      />,
    );
    expect(screen.getByTestId('inventory-ingestion-banner')).toBeVisible();
    const steps = screen.getByTestId('inventory-ingestion-steps');
    expect(steps).toHaveTextContent('Reading the source files');
    expect(steps).toHaveTextContent('Extracting entities');
  });

  it('keeps the summary and names every object the run wrote', () => {
    renderWithProviders(
      <IngestionBanner
        run={run({
          summary: 'Ingestion completed for github:9110: 6 entities, 5 relations from 12 files.',
          artifacts: [
            { name: 'graph.json', objectType: 'knowledge_graph' },
            { name: 'sources_status.json', objectType: 'sources_status' },
            { name: '.ingestion-checkpoint-github:9110.json', objectType: 'ingestion_checkpoint' },
          ],
        })}
      />,
    );
    expect(screen.getByTestId('inventory-ingestion-summary')).toHaveTextContent('6 entities, 5 relations');
    const artifacts = screen.getAllByTestId('inventory-ingestion-artifact');
    expect(artifacts.map((node) => node.textContent)).toEqual([
      'graph.json',
      'sources_status.json',
      '.ingestion-checkpoint-github:9110.json',
    ]);
    // The checkpoint is the one nothing else reports, so its kind is on the row.
    expect(artifacts[2]).toHaveAttribute('data-object-type', 'ingestion_checkpoint');
  });

  it('reports a failed run rather than falling silent', () => {
    renderWithProviders(<IngestionBanner run={run({ error: 'This deployment may not clone from that host.' })} />);
    expect(screen.getByText('This deployment may not clone from that host.')).toBeVisible();
  });
});
