/**
 * The platform default model card (#6826).
 *
 * The assertions are the three things an admin does with it — read the stored
 * choice, choose another, clear it — and the state the issue's acceptance
 * criterion names: a stored model that is no longer available must ask for a
 * replacement instead of failing silently.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { PlatformDefaultModelCard, platformDefaultSelection } from './PlatformDefaultModelCard';
import { normalisePlatformDefault } from './api/adminPlatformDefaultModelApi';
import { platformDefaultImpact } from './platformDefaultImpact';
import { renderAdminRoute } from './__tests__/testRouter';

const CANDIDATES = [
  { name: 'gpt-4o-mini', display_name: 'GPT-4o mini' },
  { name: 'gpt-4o', display_name: 'GPT-4o' },
];

function useDefault(body: Record<string, unknown>): void {
  server.use(
    http.get('*/admin/gateway/default_model', () =>
      HttpResponse.json({ model_project_id: 1, available: true, candidates: CANDIDATES, ...body }),
    ),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('PlatformDefaultModelCard', () => {
  it('shows the stored platform default', async () => {
    useDefault({ model_name: 'gpt-4o-mini' });
    renderAdminRoute(<PlatformDefaultModelCard />);

    const select = await screen.findByRole('combobox', { name: 'Default model' });
    await waitFor(() => {
      expect(select).toHaveTextContent('GPT-4o mini');
    });
    expect(screen.queryByTestId('platform-default-model-unavailable')).toBeNull();
    // Nothing changed yet, so there is nothing to save.
    expect(screen.getByTestId('platform-default-model-save')).toBeDisabled();
  });

  it('saves the chosen model', async () => {
    useDefault({ model_name: 'gpt-4o-mini' });
    const bodies: unknown[] = [];
    server.use(
      http.put('*/admin/gateway/default_model', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ model_name: 'gpt-4o', model_project_id: 1, available: true, candidates: CANDIDATES });
      }),
    );
    renderAdminRoute(<PlatformDefaultModelCard />);

    await userEvent.click(await screen.findByRole('combobox', { name: 'Default model' }));
    await userEvent.click(await screen.findByRole('option', { name: 'GPT-4o' }));
    await userEvent.click(screen.getByTestId('platform-default-model-save'));

    await waitFor(() => {
      expect(bodies).toEqual([{ model_name: 'gpt-4o' }]);
    });
    await waitFor(() => {
      expect(screen.getByTestId('platform-default-model-save')).toBeDisabled();
    });
  });

  it('asks for a replacement when the stored model is no longer available', async () => {
    useDefault({ model_name: 'retired-model', available: false });
    renderAdminRoute(<PlatformDefaultModelCard />);

    const warning = await screen.findByTestId('platform-default-model-unavailable');
    expect(warning).toHaveTextContent('retired-model');
    expect(warning).toHaveTextContent('Choose a replacement');
    // The select does not pretend the retired model is still chosen.
    expect(screen.getByTestId('platform-default-model-save')).toBeDisabled();
  });

  it('clears the platform default', async () => {
    useDefault({ model_name: 'gpt-4o' });
    let cleared = 0;
    server.use(
      http.delete('*/admin/gateway/default_model', () => {
        cleared += 1;
        return HttpResponse.json({ model_name: '', model_project_id: null, available: true, candidates: CANDIDATES });
      }),
    );
    renderAdminRoute(<PlatformDefaultModelCard />);

    const clear = await screen.findByTestId('platform-default-model-clear');
    await waitFor(() => {
      expect(clear).toBeEnabled();
    });
    await userEvent.click(clear);
    await waitFor(() => {
      expect(cleared).toBe(1);
    });
    await waitFor(() => {
      expect(screen.getByTestId('platform-default-model-clear')).toBeDisabled();
    });
  });

  it('shows the server refusal of a narrower model', async () => {
    useDefault({ model_name: '' });
    server.use(
      http.put('*/admin/gateway/default_model', () =>
        HttpResponse.json({ error: 'choose a platform model that is shared and available to all projects.' }, { status: 400 }),
      ),
    );
    renderAdminRoute(<PlatformDefaultModelCard />);

    await userEvent.click(await screen.findByRole('combobox', { name: 'Default model' }));
    await userEvent.click(await screen.findByRole('option', { name: 'GPT-4o' }));
    await userEvent.click(screen.getByTestId('platform-default-model-save'));

    expect(await screen.findByTestId('platform-default-model-error')).toHaveTextContent('available to all projects');
  });
});

describe('platform default helpers', () => {
  it('selects the stored model only while it is available', () => {
    const view = normalisePlatformDefault({ model_name: 'gpt-4o', candidates: CANDIDATES });
    expect(platformDefaultSelection(view, undefined)).toBe('gpt-4o');
    expect(platformDefaultSelection({ ...view, available: false }, undefined)).toBe('');
    expect(platformDefaultSelection(view, 'gpt-4o-mini')).toBe('gpt-4o-mini');
    expect(platformDefaultSelection(undefined, undefined)).toBe('');
  });

  it('fills an absent body', () => {
    expect(normalisePlatformDefault(undefined)).toEqual({
      model_name: '',
      model_project_id: null,
      available: true,
      candidates: [],
    });
  });

  it('describes who falls back after a delete', () => {
    expect(platformDefaultImpact(undefined)).toBeUndefined();
    expect(platformDefaultImpact({ model_name: 'm', platform_default: false, projects: 0 })).toBeUndefined();
    expect(platformDefaultImpact({ model_name: 'm', platform_default: false, projects: 2 })).toContain(
      '2 projects chose it',
    );
    expect(platformDefaultImpact({ model_name: 'm', platform_default: true, projects: 0 })).toContain(
      'platform default',
    );
  });
});
