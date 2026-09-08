import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { Bucket } from '@/entities/bucket';

import { renderWithProviders } from '../__tests__/testUtils';
import { BucketSidebar } from './BucketSidebar';

const buckets: Bucket[] = [
  { id: '1', name: 'docs', isPinned: false, createdAt: '2026-01-01T00:00:00Z', retentionDays: null, sizeBytes: 1024 },
  { id: '2', name: 'images', isPinned: true, createdAt: '2026-01-02T00:00:00Z', retentionDays: 30, sizeBytes: 2048 },
];

function baseProps(): Parameters<typeof BucketSidebar>[0] {
  return {
    buckets,
    storageConfigurations: [{ id: 's1', title: 'Primary', shared: false }],
    loading: false,
    collapsed: false,
    tree: [],
    expandedPaths: [],
    totalSize: '3.0 KB',
    onToggleCollapsed: vi.fn(),
    onStorageChange: vi.fn(),
    onSelect: vi.fn(),
    onCreate: vi.fn(),
    onEdit: vi.fn(),
    onPin: vi.fn().mockResolvedValue(undefined),
    onDelete: vi.fn().mockResolvedValue(undefined),
    onSelectFile: vi.fn(),
    onSelectFolder: vi.fn(),
  };
}

function renderSidebar(overrides: Partial<Parameters<typeof BucketSidebar>[0]> = {}) {
  const props = { ...baseProps(), ...overrides };
  renderWithProviders(<BucketSidebar {...props} />);
  return props;
}

describe('BucketSidebar', () => {
  it('selects, filters, creates, and toggles pins', async () => {
    const user = userEvent.setup();
    const props = renderSidebar();
    await user.click(screen.getByText('docs'));
    expect(props.onSelect).toHaveBeenCalledWith(buckets[0]);
    await user.click(screen.getByRole('button', { name: 'Pin docs' }));
    expect(props.onPin).toHaveBeenCalledWith(buckets[0]);
    await user.click(screen.getByRole('button', { name: 'Create bucket' }));
    expect(props.onCreate).toHaveBeenCalled();
    // The search field lives behind its own button, as in the baseline — it is
    // not a permanent row above the list.
    expect(screen.queryByPlaceholderText('Search buckets')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Search buckets' }));
    await user.type(screen.getByPlaceholderText('Search buckets'), 'image');
    expect(screen.queryByText('docs')).not.toBeInTheDocument();
    expect(screen.getByText('images')).toBeInTheDocument();
  });

  /**
   * The footer is a structural element, not decoration: it is the only place
   * the panel reports how much the project is storing, and it was missing
   * entirely before the parity pass.
   */
  it('reports the bucket count and total size in the footer', () => {
    renderSidebar();
    expect(screen.getByText('Buckets:')).toBeInTheDocument();
    expect(screen.getByText('2')).toBeInTheDocument();
    expect(screen.getByText('Size:')).toBeInTheDocument();
    expect(screen.getByText('3.0 KB')).toBeInTheDocument();
  });

  /** The selected bucket's files hang under its row — `BucketContent.jsx`. */
  it('renders the selected bucket file tree and raises the file selection', async () => {
    const user = userEvent.setup();
    const props = renderSidebar({
      selectedBucket: 'docs',
      tree: [{ id: 'notes.md', key: 'notes.md', name: 'notes.md', kind: 'file', size: 12 }],
    });
    await user.click(screen.getByRole('button', { name: 'notes.md' }));
    expect(props.onSelectFile).toHaveBeenCalledWith(expect.objectContaining({ key: 'notes.md' }));
  });

  it('collapses the panel', async () => {
    const user = userEvent.setup();
    const props = renderSidebar();
    await user.click(screen.getByRole('button', { name: 'Collapse buckets panel' }));
    expect(props.onToggleCollapsed).toHaveBeenCalled();
  });

  it('hides the title, list and footer when collapsed', () => {
    renderSidebar({ collapsed: true });
    expect(screen.queryByText('Buckets:')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Expand buckets panel' })).toBeInTheDocument();
  });

  it('offers no rename affordance — S3 buckets cannot be renamed in place', () => {
    renderSidebar();
    expect(screen.queryByRole('button', { name: 'Rename docs' })).not.toBeInTheDocument();
  });

  /**
   * The edit affordance the baseline reaches through `Buckets.jsx`'s
   * `handleEdit`. It hands the WHOLE bucket up, not just the name — the
   * caller needs the row to build the create-bucket link.
   */
  it('raises onEdit for the clicked bucket', async () => {
    const user = userEvent.setup();
    const props = renderSidebar();
    await user.click(screen.getByRole('button', { name: 'Edit images' }));
    expect(props.onEdit).toHaveBeenCalledWith(buckets[1]);
  });

  it('deletes through a confirmation dialog', async () => {
    const user = userEvent.setup();
    const props = renderSidebar();
    await user.click(screen.getByRole('button', { name: 'Delete docs' }));
    await user.click(screen.getByRole('button', { name: 'Delete' }));
    expect(props.onDelete).toHaveBeenCalledWith(buckets[0]);
  });

  it('shows loading and empty states and changes storage', async () => {
    const user = userEvent.setup();
    const props = renderSidebar({ buckets: [], loading: true });
    expect(screen.getByText('Loading buckets…')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Create bucket' }));
    expect(props.onCreate).toHaveBeenCalled();
  });

  it('renders the storage selector even with no configured storage', () => {
    renderSidebar({ buckets: [], storageConfigurations: [] });
    expect(screen.getByText('No buckets found.')).toBeInTheDocument();
    // The row keeps its place in the panel's geometry; dropping it moved
    // everything below up by its height plus its border.
    expect(screen.getByRole('button', { name: 'Storage integration' })).toBeInTheDocument();
    expect(screen.getByText('Select Storage')).toBeInTheDocument();
  });
});
