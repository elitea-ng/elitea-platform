/**
 * #6732: a member budget refusal links to `/settings/usage?scope=user`. The
 * page must then read the caller's OWN member budget, not the project's. Before
 * the fix the page always asked for `scope=project`, so a member whose own cap
 * refused the turn saw the project's spend, possibly well under its limit.
 */
import { render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import * as runtimeConfig from '@/shared/config';
import { server } from '@/test/setup';
import { paramSchemas } from '@/routes/-search/params';

import Usage from './Usage';

const BASE = '/api/v2';
const PROJECT = '7';

const requested: string[] = [];

beforeEach(() => {
  requested.length = 0;
  configureGeneratedClient({ baseUrl: BASE });
  vi.spyOn(runtimeConfig, 'getConfig').mockReturnValue({
    status: 'ok',
    config: {
      vite_server_url: BASE,
      vite_base_uri: '/',
      vite_public_project_id: '1',
      allow_project_own_llms: false,
    },
  });
  server.use(
    http.get(`${BASE}/elitea_core/usage/prompt_lib/:projectId/usage`, ({ request }) => {
      const scope = new URL(request.url).searchParams.get('scope');
      requested.push(scope ?? '');
      return HttpResponse.json({
        scope,
        can_see_amounts: true,
        spend: scope === 'user' ? 9.5 : 1.25,
        effective_limit: 10,
        remaining: scope === 'user' ? 0.5 : 8.75,
        percent_used: scope === 'user' ? 95 : 12.5,
        warning_pct: 80,
        spend_available: true,
      });
    }),
  );
});

afterEach(() => {
  vi.restoreAllMocks();
  resetGeneratedClient();
});

describe('Settings > Usage scope (#6732)', () => {
  it('reads the caller own member budget for scope=user', async () => {
    render(
      <AppProviders>
        <Usage projectId={PROJECT} scope="user" />
      </AppProviders>,
    );

    expect(await screen.findByText('$9.50')).toBeInTheDocument();
    expect(requested).toEqual(['user']);
    expect(screen.getByTestId('settings-usage-member-scope')).toBeInTheDocument();
    expect(screen.getByTestId('settings-usage')).toHaveAttribute('data-scope', 'user');
  });

  it('keeps the project read by default', async () => {
    render(
      <AppProviders>
        <Usage projectId={PROJECT} />
      </AppProviders>,
    );

    expect(await screen.findByText('$1.25')).toBeInTheDocument();
    await waitFor(() => expect(requested).toEqual(['project']));
    expect(screen.queryByTestId('settings-usage-member-scope')).not.toBeInTheDocument();
  });

  it('accepts only project or user in the URL', () => {
    expect(paramSchemas.scope.parse('user')).toBe('user');
    expect(paramSchemas.scope.parse(undefined)).toBe('project');
    expect(paramSchemas.scope.parse('admin')).toBe('project');
  });
});
