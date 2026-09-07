/**
 * The bucket-access COMPOSITION ROOT, exercised through the control an
 * operator actually presses.
 *
 * WHY THIS TEST AND NOT A DIALOG TEST. `BucketAccessDialog` renders whatever
 * rows it is handed, so a test of the dialog alone stays green when nothing
 * fetches any rows, when the wrong project id is passed, and when the save
 * button reaches no endpoint. That is the defect class #597 records: both
 * halves correct, the wiring the bug. So this file starts at `BucketSidebar`
 * — the component the page mounts — clicks "Manage access", and asserts
 * against what the SERVER was asked for and what it was sent.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { Bucket } from '@/entities/bucket';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithProviders } from '../__tests__/testUtils';
import { BucketSidebar } from './BucketSidebar';

const BASE = '/api/v2';

const buckets: Bucket[] = [
  { id: '1', name: 'reports', isPinned: false, createdAt: '2026-01-01T00:00:00Z', retentionDays: null, sizeBytes: 1024 },
];

function sidebarProps(): Parameters<typeof BucketSidebar>[0] {
  return {
    projectId: '7',
    buckets,
    storageConfigurations: [],
    loading: false,
    collapsed: false,
    tree: [],
    expandedPaths: [],
    totalSize: '1.0 KB',
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

/** The two reads the panel performs, with the bodies the routes answer. */
function seedReads(rows: unknown[]): void {
  server.use(
    http.get(`${BASE}/artifacts/bucket_permissions/7`, () =>
      HttpResponse.json({ total: rows.length, rows })),
    http.get(`${BASE}/admin/users/default/7`, () =>
      HttpResponse.json({
        total: 2,
        rows: [
          { id: '11', email: 'blocked@example.test', name: 'Blocked Member', roles: [] },
          { id: '12', email: 'free@example.test', name: 'Free Member', roles: [] },
        ],
      })),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('BucketAccessPanel, through the bucket row', () => {
  it('fetches and renders the bucket’s exceptions when "Manage access" is pressed', async () => {
    seedReads([
      { user_id: 11, name: 'Blocked Member', email: 'blocked@example.test', bucket_permissions: { reports: [] } },
    ]);
    renderWithProviders(<BucketSidebar {...sidebarProps()} />);

    // The dialog does not exist until the control is used — the panel returns
    // null while no bucket is selected, so a mounted-but-hidden dialog would
    // be a different (and wrong) wiring.
    expect(screen.queryByTestId('bucket-access-dialog')).toBeNull();

    await userEvent.click(screen.getByLabelText('Manage access to reports'));

    expect(await screen.findByTestId('bucket-access-row-11')).toBeInTheDocument();
    expect(screen.getByText('blocked@example.test')).toBeInTheDocument();
    // The stored `[]` must read as "No access", not as the default. Both are
    // falsy, and the whole point of the mapping layer is that they cannot be
    // confused here.
    // The rendered LABEL, not the value: a MUI Select's accessible node is the
    // combobox, and what the operator reads is the option text.
    expect(screen.getByLabelText('Permissions for Blocked Member')).toHaveTextContent('No access');
  });

  it('says so, and shows no rows, when the project restricts nobody', async () => {
    seedReads([]);
    renderWithProviders(<BucketSidebar {...sidebarProps()} />);
    await userEvent.click(screen.getByLabelText('Manage access to reports'));

    expect(await screen.findByTestId('bucket-access-empty')).toBeInTheDocument();
  });

  // A member whose map names ANOTHER bucket has no exception here.
  it('does not list an exception that belongs to a different bucket', async () => {
    seedReads([
      { user_id: 11, name: 'Blocked Member', email: 'blocked@example.test', bucket_permissions: { datasets: [] } },
    ]);
    renderWithProviders(<BucketSidebar {...sidebarProps()} />);
    await userEvent.click(screen.getByLabelText('Manage access to reports'));

    expect(await screen.findByTestId('bucket-access-empty')).toBeInTheDocument();
    expect(screen.queryByTestId('bucket-access-row-11')).toBeNull();
  });

  // The route REPLACES the member's map, so the request must carry the
  // exceptions the dialog did not touch. Sending only the edited bucket would
  // silently clear the rest, and every screen would still look right.
  it('sends the member’s other exceptions back when it changes one bucket', async () => {
    seedReads([
      {
        user_id: 11,
        name: 'Blocked Member',
        email: 'blocked@example.test',
        bucket_permissions: { reports: [], datasets: ['read'] },
      },
    ]);
    let sent: unknown;
    server.use(
      http.put(`${BASE}/artifacts/bucket_permissions/7`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json({ user_id: 11, bucket_permissions: {} });
      }),
    );
    renderWithProviders(<BucketSidebar {...sidebarProps()} />);
    await userEvent.click(screen.getByLabelText('Manage access to reports'));
    await screen.findByTestId('bucket-access-row-11');

    await userEvent.click(screen.getByLabelText('Permissions for Blocked Member'));
    await userEvent.click(await screen.findByRole('option', { name: 'Read-only' }));

    await waitFor(() => {
      expect(sent).toEqual({
        user_id: 11,
        bucket_permissions: { reports: ['read'], datasets: ['read'] },
      });
    });
  });

  // "Read/write (default)" is a REMOVAL, not a stored grant.
  it('removes the entry when the default is chosen', async () => {
    seedReads([
      { user_id: 11, name: 'Blocked Member', email: 'blocked@example.test', bucket_permissions: { reports: [] } },
    ]);
    let sent: unknown;
    server.use(
      http.put(`${BASE}/artifacts/bucket_permissions/7`, async ({ request }) => {
        sent = await request.json();
        return HttpResponse.json({ user_id: 11, bucket_permissions: {} });
      }),
    );
    renderWithProviders(<BucketSidebar {...sidebarProps()} />);
    await userEvent.click(screen.getByLabelText('Manage access to reports'));
    await screen.findByTestId('bucket-access-row-11');

    await userEvent.click(screen.getByLabelText('Permissions for Blocked Member'));
    await userEvent.click(await screen.findByRole('option', { name: 'Read/write (default)' }));

    await waitFor(() => {
      expect(sent).toEqual({ user_id: 11, bucket_permissions: {} });
    });
  });

  it('deletes one exception through the DELETE route', async () => {
    seedReads([
      { user_id: 11, name: 'Blocked Member', email: 'blocked@example.test', bucket_permissions: { reports: [] } },
    ]);
    let sent: unknown;
    server.use(
      http.delete(`${BASE}/artifacts/bucket_permissions/7`, async ({ request }) => {
        sent = await request.json();
        return new HttpResponse(null, { status: 204 });
      }),
    );
    renderWithProviders(<BucketSidebar {...sidebarProps()} />);
    await userEvent.click(screen.getByLabelText('Manage access to reports'));
    await screen.findByTestId('bucket-access-row-11');

    await userEvent.click(screen.getByLabelText('Remove exception for Blocked Member'));

    await waitFor(() => {
      expect(sent).toEqual({ user_id: 11, bucket: 'reports' });
    });
  });

  it('shows the failure rather than reporting success when the save is refused', async () => {
    seedReads([
      { user_id: 11, name: 'Blocked Member', email: 'blocked@example.test', bucket_permissions: { reports: [] } },
    ]);
    server.use(
      http.put(`${BASE}/artifacts/bucket_permissions/7`, () =>
        HttpResponse.json({ error: { code: 'Forbidden', message: 'no' } }, { status: 403 })),
    );
    renderWithProviders(<BucketSidebar {...sidebarProps()} />);
    await userEvent.click(screen.getByLabelText('Manage access to reports'));
    await screen.findByTestId('bucket-access-row-11');

    await userEvent.click(screen.getByLabelText('Permissions for Blocked Member'));
    await userEvent.click(await screen.findByRole('option', { name: 'Read-only' }));

    expect(await screen.findByTestId('bucket-access-error')).toBeInTheDocument();
  });
});
