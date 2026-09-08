import { fireEvent, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import type { Artifact } from '@/entities/artifact';

import { renderWithProviders } from '../__tests__/testUtils';
import { ArtifactTable } from './ArtifactTable';

const contents: Artifact[] = [
  { key: 'folder/deep.txt', size: 20, lastModified: '2026-01-02T00:00:00Z', bucket: 'docs' },
  { key: 'alpha.txt', size: 10, lastModified: '2026-01-01T00:00:00Z', bucket: 'docs' },
  { key: 'beta.svg', size: 30, lastModified: '2026-01-03T00:00:00Z', bucket: 'docs' },
];

function renderTable(overrides: Partial<Parameters<typeof ArtifactTable>[0]> = {}) {
  const props = {
    bucket: 'docs',
    retentionDays: null,
    contents,
    currentPrefix: '',
    loading: false,
    onPrefixChange: vi.fn(),
    onPreview: vi.fn(),
    onDownload: vi.fn(),
    onDownloadMany: vi.fn(),
    onDelete: vi.fn(),
    onUpload: vi.fn(),
    ...overrides,
  };
  const view = renderWithProviders(<ArtifactTable {...props} />);
  return { ...props, container: view.container };
}

describe('ArtifactTable', () => {
  it('renders folders and files and routes row actions', async () => {
    const user = userEvent.setup();
    const props = renderTable();
    // A folder row navigates on click; a file row does not (only its own
    // preview button opens it) — `handleRowClick` in the baseline.
    await user.click(screen.getByText('folder'));
    expect(props.onPrefixChange).toHaveBeenCalledWith('folder/');
    await user.click(screen.getByRole('button', { name: 'Preview alpha.txt' }));
    expect(props.onPreview).toHaveBeenCalledWith(expect.objectContaining({ key: 'alpha.txt' }));
    await user.click(screen.getByRole('button', { name: 'Download alpha.txt' }));
    expect(props.onDownload).toHaveBeenCalledWith(expect.objectContaining({ key: 'alpha.txt' }));
    await user.click(screen.getByRole('button', { name: 'Delete beta.svg' }));
    expect(props.onDelete).toHaveBeenCalledWith([expect.objectContaining({ key: 'beta.svg' })]);
  });

  /**
   * The `Type` column is a real column, not a label: it prints the baseline's
   * `getFileTypeName`, so `beta.svg` reads "SVG" and not "svg".
   */
  it('renders the bucket name as the heading and a Type column', () => {
    renderTable();
    expect(screen.getByRole('heading', { name: 'docs' })).toBeInTheDocument();
    expect(screen.getByRole('columnheader', { name: /Type/ })).toBeInTheDocument();
    expect(screen.getByText('SVG')).toBeInTheDocument();
    expect(screen.getByText('Text')).toBeInTheDocument();
  });

  it('searches, sorts, selects, and invokes bulk actions', async () => {
    const user = userEvent.setup();
    const props = renderTable();
    await user.type(screen.getByPlaceholderText('Search'), 'alpha');
    expect(screen.getByText('alpha.txt')).toBeInTheDocument();
    expect(screen.queryByText('beta.svg')).not.toBeInTheDocument();
    await user.clear(screen.getByPlaceholderText('Search'));
    await user.click(screen.getByRole('button', { name: 'Size' }));
    await user.click(screen.getByRole('checkbox', { name: 'Select alpha.txt' }));
    await user.click(screen.getByRole('button', { name: 'Download selected' }));
    await user.click(screen.getByRole('button', { name: 'Delete selected' }));
    expect(props.onDownloadMany).toHaveBeenCalledWith([expect.objectContaining({ key: 'alpha.txt' })]);
    expect(props.onDelete).toHaveBeenCalledWith([expect.objectContaining({ key: 'alpha.txt' })]);
  });

  it('stages files from the picker and drag-and-drop', async () => {
    const user = userEvent.setup();
    const props = renderTable();
    const file = new File(['hello'], 'hello.txt', { type: 'text/plain' });
    const input = document.querySelector<HTMLInputElement>('input[type="file"]');
    expect(input).not.toBeNull();
    if (input === null) throw new Error('Expected file input');
    await user.upload(input, file);
    expect(props.onUpload).toHaveBeenCalledWith([file]);
    const dropTarget = props.container.firstElementChild;
    expect(dropTarget).not.toBeNull();
    if (dropTarget === null) throw new Error('Expected drop target');
    fireEvent.drop(dropTarget, {
      dataTransfer: { files: [file] },
    });
    expect(props.onUpload).toHaveBeenCalledTimes(2);
  });

  /**
   * The picker test above drives the hidden input directly, so it says nothing
   * about the BUTTONS that are supposed to open it. That matters here: the
   * empty-state button disappears as soon as the bucket holds one file, so the
   * toolbar button is the only upload affordance a non-empty bucket has, and
   * a broken one would leave no way to add a second file at all.
   */
  it('opens the picker from the toolbar button while the bucket is NOT empty', async () => {
    const user = userEvent.setup();
    renderTable();
    const input = document.querySelector<HTMLInputElement>('input[type="file"]');
    if (input === null) throw new Error('Expected file input');
    const opened = vi.spyOn(input, 'click');

    expect(screen.queryByText('No files in this bucket')).toBeNull();
    await user.click(screen.getByRole('button', { name: 'Upload files' }));

    expect(opened).toHaveBeenCalledTimes(1);
  });

  it('opens the same picker from the empty-state button', async () => {
    const user = userEvent.setup();
    renderTable({ contents: [] });
    const input = document.querySelector<HTMLInputElement>('input[type="file"]');
    if (input === null) throw new Error('Expected file input');
    const opened = vi.spyOn(input, 'click');

    // Two controls with the same accessible name — the toolbar's icon button
    // and the empty state's — so this picks the one inside the empty state.
    const emptyStateButton = screen.getByText('No files in this bucket').parentElement?.querySelector('button');
    if (!(emptyStateButton instanceof HTMLElement)) throw new Error('Expected empty-state upload button');
    await user.click(emptyStateButton);

    expect(opened).toHaveBeenCalledTimes(1);
  });

  it('supports select-all, repeated sorting, and folder row navigation', async () => {
    const user = userEvent.setup();
    const props = renderTable();
    await user.click(screen.getByRole('checkbox', { name: 'Select all artifacts' }));
    await user.click(screen.getByRole('button', { name: 'Download selected' }));
    expect(props.onDownloadMany).toHaveBeenCalledWith(expect.arrayContaining([
      expect.objectContaining({ key: 'folder/' }),
      expect.objectContaining({ key: 'alpha.txt' }),
      expect.objectContaining({ key: 'beta.svg' }),
    ]));
    await user.click(screen.getByRole('button', { name: 'Name' }));
    await user.click(screen.getByRole('button', { name: 'Name' }));
    // A real <tr>, not a role attribute: `prefer-tag-over-role` (R-C1) wants
    // the semantic tag, and Playwright's `getByRole('row')` (J20d) reads it
    // the same either way.
    const folderRow = screen.getByText('folder').closest('tr');
    expect(folderRow).toBeInstanceOf(HTMLElement);
    if (!(folderRow instanceof HTMLElement)) throw new Error('Expected folder row');
    await user.click(folderRow);
    expect(props.onPrefixChange).toHaveBeenCalledWith('folder/');
  });

  /** The footer is the baseline's own, not MUI's: "1 - 3 of 3", never "1–3 of 3". */
  it('renders the baseline pagination footer', () => {
    renderTable();
    expect(screen.getByText('Rows per page:')).toBeInTheDocument();
    expect(screen.getByText('1 - 3 of 3')).toBeInTheDocument();
  });

  it('renders loading, error, breadcrumbs, and empty states', async () => {
    const user = userEvent.setup();
    const props = renderTable({
      contents: [],
      currentPrefix: 'a/b/',
      loading: false,
      error: 'Load failed',
    });
    expect(screen.getByRole('alert')).toHaveTextContent('Load failed');
    expect(screen.getByText('No files in this bucket')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'docs' }));
    expect(props.onPrefixChange).toHaveBeenCalledWith('');
  });
});
