/**
 * Settings › General › Send to cloud (ADR-0034 decision 11): the project's
 * own cloud executor on/off over `/elitea_core/project_cloud_executor`.
 *
 * What has to hold: the section shows the effective state and whether it is
 * the project's choice or the default for projects; the project admin
 * (`models.project_settings.edit`) can turn it on, off, or back to the
 * default (`enabled: null`); an editor sees it read-only (no personal-project
 * exception); and while the platform switch is off the whole control is
 * disabled with the reason.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { getGetAdminProjectCloudExecutorQueryKey } from '@/shared/api/generated/admin/admin';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '@/test/setup';

import { ProjectGeneral } from './ProjectGeneral';

const BASE = '/api/v2';

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

function settle(next: Partial<SwitchState>): SwitchState {
  const merged = { ...state, ...next };
  return {
    ...merged,
    source: merged.explicit === null ? 'default' : 'explicit',
    enabled: merged.platform_enabled && (merged.explicit ?? merged.project_default_enabled),
  };
}

function grant(permissions: readonly string[]) {
  server.use(
    http.get(`${BASE}/auth/permissions/prompt_lib/:projectId`, () =>
      HttpResponse.json(permissions.map((name) => ({ name, enabled: true }))),
    ),
  );
}

function mount(projectName = 'Team Alpha', client = new QueryClient({ defaultOptions: { queries: { retry: false } } })) {
  renderWithTheme(
    <QueryClientProvider client={client}>
      <ProjectGeneral projectId="7" projectName={projectName} />
    </QueryClientProvider>,
  );
}

function section(): HTMLElement {
  return screen.getByTestId('cloud-executor-section');
}

beforeEach(() => {
  writes = [];
  state = settle({ project_id: 7, platform_enabled: true, project_default_enabled: true, explicit: null });
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/project_info/prompt_lib/:projectId/project-info`, () =>
      HttpResponse.json({ name: 'Demo', icon_meta: null, teammates_count: 3 }),
    ),
    http.get(`${BASE}/elitea_core/project_cloud_executor/prompt_lib/:projectId`, () => HttpResponse.json(state)),
    http.put(`${BASE}/elitea_core/project_cloud_executor/prompt_lib/:projectId`, async ({ request }) => {
      const body = (await request.json()) as { enabled: boolean | null };
      writes.push(body);
      state = settle({ explicit: body.enabled });
      return HttpResponse.json(state);
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('Settings › General — Send to cloud', () => {
  it('lets the project admin turn it off and go back to the default', async () => {
    grant(['models.project_settings.edit', 'models.project_context.edit']);
    const user = userEvent.setup();
    mount();

    expect(await within(section()).findByTestId('cloud-executor-switch-status')).toHaveTextContent('On');
    expect(within(section()).getByTestId('cloud-executor-switch-source')).toHaveTextContent(
      'Following the default for projects',
    );
    await waitFor(() => {
      expect(within(section()).getByRole('radio', { name: 'Off' })).toBeEnabled();
    });

    await user.click(within(section()).getByRole('radio', { name: 'Off' }));
    await waitFor(() => {
      expect(within(section()).getByTestId('cloud-executor-switch-status')).toHaveTextContent('Off');
    });
    expect(within(section()).getByTestId('cloud-executor-switch-source')).toHaveTextContent('Set for this project');

    await user.click(within(section()).getByRole('radio', { name: /Use the default for projects/ }));
    await waitFor(() => {
      expect(writes).toEqual([{ enabled: false }, { enabled: null }]);
    });
    await waitFor(() => {
      expect(within(section()).getByTestId('cloud-executor-switch-status')).toHaveTextContent('On');
    });
  });

  it('shows an editor the state read-only, even in a personal project', async () => {
    grant(['models.project_context.edit', 'models.project_context.view']);
    mount('project_user_42');

    expect(await within(section()).findByRole('radio', { name: 'Off' })).toBeDisabled();
    expect(within(section()).getByText('Only a project admin can change this.')).toBeInTheDocument();
  });

  it('is disabled with the reason while the platform switch is off', async () => {
    state = settle({ platform_enabled: false, explicit: true });
    grant(['models.project_settings.edit']);
    mount();

    expect(await within(section()).findByText(/turned off for this deployment/)).toBeInTheDocument();
    expect(within(section()).getByTestId('cloud-executor-switch-status')).toHaveTextContent('Off');
    expect(within(section()).getByTestId('cloud-executor-switch-source')).toHaveTextContent('Off for this deployment');
    for (const name of [/Use the default for projects/, 'On', 'Off']) {
      expect(within(section()).getByRole('radio', { name })).toBeDisabled();
    }
  });

  it('updates the admin Projects cache of the same switch too (PR #1319 review)', async () => {
    // Admin › Projects caches the same switch under its own key; a save here
    // must not leave that dialog showing the old value.
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const adminKey = getGetAdminProjectCloudExecutorQueryKey('7');
    client.setQueryData(adminKey, { data: state, status: 200 });
    grant(['models.project_settings.edit']);
    const user = userEvent.setup();
    mount('Team Alpha', client);

    await waitFor(() => {
      expect(within(section()).getByRole('radio', { name: 'Off' })).toBeEnabled();
    });
    await user.click(within(section()).getByRole('radio', { name: 'Off' }));

    await waitFor(() => {
      expect(client.getQueryData<{ data: SwitchState }>(adminKey)?.data).toMatchObject({
        explicit: false,
        enabled: false,
        source: 'explicit',
      });
    });
  });
});
