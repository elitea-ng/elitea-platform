/**
 * Admin › Projects › Send to cloud (ADR-0034 decision 11): one project's
 * cloud executor switch over `/admin/project_cloud_executor/administration`.
 *
 * What has to hold: the dialog shows the effective state and where it comes
 * from (the project's own choice or the default for projects); each choice
 * writes `{enabled}` with `null` for "Use the default for projects"; "On" is
 * not offered while the platform switch is off; an operator without the
 * write sees the state and cannot change it; a refused write says why.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { QueryClient } from '@tanstack/react-query';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { getGetProjectCloudExecutorQueryKey } from '@/shared/api/generated/settings/settings';
import { server } from '@/test/setup';

import { AdminProjectCloudExecutorDialog } from './AdminProjectCloudExecutorDialog';
import type { AdminProjectRow } from './api/adminProjectsApi';
import { renderAdminRoute } from './__tests__/testRouter';

const PROJECT: AdminProjectRow = {
  id: 42,
  name: 'Payments',
  owner_id: 1,
  owner_name: 'Owner',
  admin_names: [],
  status: 'active',
  suspended: false,
  create_success: true,
  is_personal: false,
};

interface SwitchState {
  project_id: number;
  platform_enabled: boolean;
  project_default_enabled: boolean;
  explicit: boolean | null;
  source: 'explicit' | 'default';
  enabled: boolean;
}

let state: SwitchState;
let writes: unknown[];
let refuseWith: number | undefined;

function settle(next: Partial<SwitchState>): SwitchState {
  const merged = { ...state, ...next };
  const own = merged.explicit ?? merged.project_default_enabled;
  return {
    ...merged,
    source: merged.explicit === null ? 'default' : 'explicit',
    enabled: merged.platform_enabled && own,
  };
}

beforeEach(() => {
  writes = [];
  refuseWith = undefined;
  state = settle({
    project_id: 42,
    platform_enabled: true,
    project_default_enabled: false,
    explicit: null,
  });
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('*/admin/project_cloud_executor/administration/:projectId', () => HttpResponse.json(state)),
    http.put('*/admin/project_cloud_executor/administration/:projectId', async ({ request }) => {
      const body = (await request.json()) as { enabled: boolean | null };
      writes.push(body);
      if (refuseWith) return HttpResponse.json({ error: 'refused' }, { status: refuseWith });
      state = settle({ explicit: body.enabled });
      return HttpResponse.json(state);
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

function open(canEdit = true, queryClient?: QueryClient) {
  return renderAdminRoute(<AdminProjectCloudExecutorDialog project={PROJECT} canEdit={canEdit} onClose={() => undefined} />, queryClient ? { queryClient } : {});
}

describe('AdminProjectCloudExecutorDialog', () => {
  it('shows the default-driven state and turns the project on explicitly', async () => {
    const user = userEvent.setup();
    open();

    expect(await screen.findByTestId('cloud-executor-switch-status')).toHaveTextContent('Off');
    expect(screen.getByTestId('cloud-executor-switch-source')).toHaveTextContent('Following the default for projects');
    expect(screen.getByRole('radio', { name: /Use the default for projects/ })).toBeChecked();
    expect(screen.getByText('The default is off')).toBeInTheDocument();

    await user.click(screen.getByRole('radio', { name: 'On' }));

    await waitFor(() => {
      expect(screen.getByTestId('cloud-executor-switch-status')).toHaveTextContent('On');
    });
    expect(writes).toEqual([{ enabled: true }]);
    expect(screen.getByTestId('cloud-executor-switch-source')).toHaveTextContent('Set for this project');
    expect(screen.getByRole('radio', { name: 'On' })).toBeChecked();
  });

  it('goes back to the default with enabled: null', async () => {
    state = settle({ project_default_enabled: true, explicit: false });
    const user = userEvent.setup();
    open();

    expect(await screen.findByRole('radio', { name: 'Off' })).toBeChecked();
    await user.click(screen.getByRole('radio', { name: /Use the default for projects/ }));

    await waitFor(() => {
      expect(writes).toEqual([{ enabled: null }]);
    });
    await waitFor(() => {
      expect(screen.getByTestId('cloud-executor-switch-status')).toHaveTextContent('On');
    });
    expect(screen.getByTestId('cloud-executor-switch-source')).toHaveTextContent('Following the default for projects');
  });

  it('does not offer On while the platform is off, but still takes Off', async () => {
    state = settle({ platform_enabled: false });
    const user = userEvent.setup();
    open();

    expect(await screen.findByText(/turned off for this deployment/)).toBeInTheDocument();
    expect(screen.getByRole('radio', { name: 'On' })).toBeDisabled();
    expect(screen.getByRole('radio', { name: 'Off' })).toBeEnabled();
    await user.click(screen.getByRole('radio', { name: 'Off' }));
    await waitFor(() => {
      expect(writes).toEqual([{ enabled: false }]);
    });
  });

  it('is read-only without the project write', async () => {
    open(false);

    expect(await screen.findByRole('radio', { name: 'Off' })).toBeDisabled();
    expect(screen.getByRole('radio', { name: /Use the default for projects/ })).toBeDisabled();
    expect(screen.getByText('Changing it needs the permission to edit projects.')).toBeInTheDocument();
  });

  it('says why a write was refused', async () => {
    refuseWith = 409;
    const user = userEvent.setup();
    open();

    await user.click(await screen.findByRole('radio', { name: 'Off' }));
    expect(await screen.findByText(/cannot be turned on for a project/)).toBeInTheDocument();
  });

  it('updates the Settings › General cache of the same switch too (PR #1319 review)', async () => {
    // The project's own route caches the same switch under another key; a
    // save here must not leave that screen showing the old value.
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const settingsKey = getGetProjectCloudExecutorQueryKey('42');
    queryClient.setQueryData(settingsKey, { data: state, status: 200 });
    const user = userEvent.setup();
    open(true, queryClient);

    await user.click(await screen.findByRole('radio', { name: 'On' }));

    await waitFor(() => {
      expect(queryClient.getQueryData<{ data: SwitchState }>(settingsKey)?.data).toMatchObject({
        explicit: true,
        enabled: true,
        source: 'explicit',
      });
    });
  });
});
