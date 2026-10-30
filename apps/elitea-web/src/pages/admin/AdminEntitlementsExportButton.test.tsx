/**
 * "Export entitlements" (ADR-0044 decision 5, #1501): the CSV the server
 * streams is handed to the browser as is, and the control is offered only to
 * an operator whose injected permissions include `entitlements.export`.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminEntitlementsExportButton } from './AdminEntitlementsExportButton';

type ConfigWindow = Window & { admin_ui_config?: { permissions?: readonly string[] } };

const CSV =
  'user_id,user_name,display_name,user_active,scope_type,scope_id,scope_name,project_id,scope_active,role,capability,permissions,granted_via\r\n' +
  '7,a@x.io,"Smith, J",true,project,3,Alpha,3,true,admin,publish_agents,models.applications.publish.post,role\r\n';

let downloads: { blob: Blob; filename: string }[];
let queries: URLSearchParams[];
let status: number;

beforeEach(() => {
  downloads = [];
  queries = [];
  status = 200;
  let pending: Blob | undefined;
  vi.spyOn(URL, 'createObjectURL').mockImplementation((blob) => {
    pending = blob as Blob;
    return 'blob:entitlements';
  });
  vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
  vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
    if (pending !== undefined) downloads.push({ blob: pending, filename: this.download });
  });
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/api/v2/admin/entitlements', ({ request }) => {
      queries.push(new URL(request.url).searchParams);
      if (status !== 200) return HttpResponse.json({ error: 'refused' }, { status });
      return new HttpResponse(CSV, {
        headers: { 'content-type': 'text/csv; charset=utf-8', 'content-disposition': 'attachment; filename="entitlements.csv"' },
      });
    }),
  );
  (window as ConfigWindow).admin_ui_config = { permissions: ['entitlements.export'] };
});

afterEach(() => {
  resetGeneratedClient();
  delete (window as ConfigWindow).admin_ui_config;
  vi.restoreAllMocks();
});

describe('AdminEntitlementsExportButton', () => {
  it('downloads the CSV the server renders', async () => {
    const user = userEvent.setup();
    render(<AdminEntitlementsExportButton />);
    await user.click(screen.getByTestId('admin-entitlements-export'));
    await waitFor(() => expect(downloads).toHaveLength(1));
    expect(queries.at(-1)?.get('format')).toBe('csv');
    expect(downloads[0]?.filename).toBe('entitlements.csv');
    expect(await downloads[0]?.blob.text()).toBe(CSV);
  });

  it('says so when the export is refused', async () => {
    status = 503;
    const user = userEvent.setup();
    render(<AdminEntitlementsExportButton />);
    await user.click(screen.getByTestId('admin-entitlements-export'));
    expect(await screen.findByText('Failed to export entitlements.')).toBeInTheDocument();
    expect(downloads).toHaveLength(0);
  });

  it('is not offered without entitlements.export', () => {
    (window as ConfigWindow).admin_ui_config = { permissions: ['admin.auth.users'] };
    render(<AdminEntitlementsExportButton />);
    expect(screen.queryByTestId('admin-entitlements-export')).toBeNull();
  });
});
