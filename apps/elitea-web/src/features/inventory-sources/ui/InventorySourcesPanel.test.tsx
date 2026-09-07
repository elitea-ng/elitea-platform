/**
 * The Sources table and the control that ingests.
 *
 * Four states of this panel are each other's opposites on screen, and getting
 * one wrong reads as a working screen telling the user something false:
 *
 *  - a FAILED status read must still list the configured sources. Rendering
 *    "no sources configured" sends a user to add ones they already have.
 *  - a toolkit with NO llm_model must say so. Otherwise the ingest button is
 *    live and the provider refuses every click with a message about a field the
 *    user never saw.
 *  - an ORPHAN row — a source in the graph that the toolkit no longer names —
 *    has no id to send, so it must offer no ingest control at all.
 *  - a count nobody reported is `—`, not `0`: "not measured" and "none" are
 *    different facts.
 */
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { IngestionStatus, InventorySource } from '@/entities/inventory';

import { renderWithProviders } from '../__tests__/testUtils';
import { InventorySourcesPanel } from './InventorySourcesPanel';

/** Nothing is running anywhere: the state most of these tests are about. */
const IDLE: IngestionStatus = {
  running: false,
  lastStatus: '',
  entityCount: null,
  relationCount: null,
  source: '',
  message: '',
};

function source(overrides: Partial<InventorySource> = {}): InventorySource {
  return {
    toolkitId: '9010',
    name: 'Checkout repo',
    type: 'github',
    branch: 'main',
    filePatterns: '',
    excludePatterns: '',
    preset: '',
    status: 'completed',
    entityCount: 4,
    relationCount: 3,
    lastUpdated: '',
    errorMessage: '',
    ...overrides,
  };
}

function show(overrides: Partial<React.ComponentProps<typeof InventorySourcesPanel>> = {}) {
  const props = {
    sources: [source()],
    isPending: false,
    statusError: null,
    ingestion: IDLE,
    runningSourceId: null,
    canIngest: true,
    onIngest: vi.fn(),
    onStop: vi.fn(),
    onAddSource: vi.fn(),
    onRemoveSource: vi.fn(),
    ...overrides,
  };
  renderWithProviders(<InventorySourcesPanel {...props} />);
  return props;
}

describe('InventorySourcesPanel', () => {
  it('says it is reading rather than showing an empty table', () => {
    show({ isPending: true });
    expect(screen.getByTestId('inventory-sources-pending')).toBeInTheDocument();
    expect(screen.queryByTestId('inventory-sources-table')).not.toBeInTheDocument();
  });

  it('renders one row per source, with its counts and branch', () => {
    show();
    const row = screen.getByTestId('inventory-source-row');
    expect(row).toHaveAttribute('data-source-id', '9010');
    expect(within(row).getByText('Checkout repo')).toBeInTheDocument();
    expect(within(row).getByText('github')).toBeInTheDocument();
    expect(within(row).getByText('main')).toBeInTheDocument();
    expect(within(row).getByTestId('inventory-source-status')).toHaveAttribute('data-status', 'done');
  });

  it('shows an unreported count as — and not as zero', () => {
    show({ sources: [source({ entityCount: null, relationCount: null, branch: '' })] });
    const cells = within(screen.getByTestId('inventory-source-row')).getAllByText('—');
    expect(cells).toHaveLength(3);
  });

  it('offers the empty state only when there really are no sources', () => {
    show({ sources: [] });
    expect(screen.getByTestId('inventory-no-sources')).toBeInTheDocument();
  });

  it('keeps the rows when the status read failed, and says the states are unknown', () => {
    show({ statusError: 'The graph could not be read.' });
    expect(screen.getByTestId('inventory-source-row')).toBeInTheDocument();
    expect(
      screen.getByText(/The ingestion status could not be read/),
    ).toBeInTheDocument();
  });

  it('warns and disables the ingest control when no model is configured', () => {
    // The provider refuses an ingestion with no llm_model. A live button here
    // reports the refusal as a failure of the click.
    show({ canIngest: false });
    expect(screen.getByText(/configures no LLM model/)).toBeInTheDocument();
    expect(screen.getByTestId('inventory-run-ingestion')).toBeDisabled();
  });

  it('starts an ingestion for the row that was clicked', async () => {
    const user = userEvent.setup();
    const props = show({ sources: [source(), source({ toolkitId: '9011', name: 'Docs' })] });
    await user.click(screen.getAllByTestId('inventory-run-ingestion')[1] as HTMLElement);
    expect(props.onIngest).toHaveBeenCalledWith('9011');
  });

  it('disables EVERY ingest control while one run is going', () => {
    // A second concurrent ingestion is refused by the provider.
    show({
      sources: [source(), source({ toolkitId: '9011', name: 'Docs', status: '' })],
      runningSourceId: '9010',
    });
    for (const button of screen.getAllByTestId('inventory-run-ingestion')) {
      expect(button).toBeDisabled();
    }
  });

  it('shows the running row as ingesting, whatever its stored status says', () => {
    show({ sources: [source({ status: 'completed' })], runningSourceId: '9010' });
    expect(screen.getByTestId('inventory-source-status')).toHaveAttribute('data-status', 'ingesting');
  });

  it('offers the stop control only while a run is going', async () => {
    const user = userEvent.setup();
    const props = show({ runningSourceId: '9010' });
    await user.click(screen.getByTestId('inventory-stop-ingestion'));
    expect(props.onStop).toHaveBeenCalledTimes(1);
  });

  it('hides the stop control when nothing is running', () => {
    show();
    expect(screen.queryByTestId('inventory-stop-ingestion')).not.toBeInTheDocument();
  });

  it('offers no ingest control on an orphan row, which has no id to send', () => {
    show({
      sources: [source({ toolkitId: '', name: 'docs', type: '', branch: '', status: 'completed' })],
    });
    expect(screen.queryByTestId('inventory-run-ingestion')).not.toBeInTheDocument();
    expect(screen.getByText(/No longer configured/)).toBeInTheDocument();
  });

  it('does not mark an orphan row as the running one', () => {
    // Both carry an empty toolkit id; treating them as equal would show a
    // removed source as the one being ingested.
    show({ sources: [source({ toolkitId: '', name: 'docs', status: 'completed' })], runningSourceId: '' });
    expect(screen.getByTestId('inventory-source-status')).toHaveAttribute('data-status', 'done');
  });

  it('reports an ingestion the PROVIDER says is running, whoever started it', async () => {
    // A run begun in another tab, by another user, or before this page opened
    // is invisible to the local controller — and a second one against a busy
    // provider is refused.
    show({ ingestion: { ...IDLE, running: true, source: 'github:9010' } });
    expect(await screen.findByText('An ingestion is already running for github:9010.')).toBeInTheDocument();
    expect(screen.getByTestId('inventory-run-ingestion')).toBeDisabled();
  });

  it('says an ingestion is running even when the provider names no source', () => {
    show({ ingestion: { ...IDLE, running: true } });
    expect(screen.getByText('An ingestion is already running for this inventory.')).toBeInTheDocument();
  });

  it('shows the provider own sentence for a source that failed', () => {
    show({ sources: [source({ status: 'error', errorMessage: 'branch "dev" not found' })] });
    expect(screen.getByTestId('inventory-source-error')).toHaveTextContent('branch "dev" not found');
  });
});
