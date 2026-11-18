import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import type { MarketplaceBindingsState } from '@/shared/api/generated/model';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { server } from '../../../test/setup';

import { BindingsEditorDialog } from './BindingsEditorDialog';

const STATE: MarketplaceBindingsState = {
  requirements: {
    items: [
      {
        key: 'toolkit:github',
        kind: 'toolkit',
        type: 'github',
        name: 'Release bot',
        tools: ['create_release'],
        has_write_tools: true,
      },
    ],
    model_class: 'tool_calling',
    tier: 'medium',
    has_write_tools: true,
  },
  bindings: [],
  tools_enabled: false,
  awaiting_approval: false,
};

let puts: unknown[] = [];

function mock(state: MarketplaceBindingsState, put?: () => Response): void {
  puts = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  server.use(
    http.get('/api/v2/elitea_core/marketplace_bindings/prompt_lib/:projectId/:applicationId', () =>
      HttpResponse.json(state),
    ),
    http.put('/api/v2/elitea_core/marketplace_bindings/prompt_lib/:projectId/:applicationId', async ({ request }) => {
      puts.push(await request.json());
      return (
        put?.() ??
        HttpResponse.json({
          ...state,
          bindings: [{ requirement_key: 'toolkit:github', toolkit_id: '12' }],
          tools_enabled: true,
        })
      );
    }),
    http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () =>
      HttpResponse.json({ rows: [{ id: '12', type: 'github', name: 'GitHub (release bot)' }], total: 1 }),
    ),
    http.get('/api/v2/elitea_core/skills/prompt_lib/:projectId', () => HttpResponse.json({ rows: [], total: 0 })),
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  renderWithTheme(
    <QueryClientProvider client={queryClient}>
      <BindingsEditorDialog open onClose={() => undefined} projectId="5" applicationId={88} />
    </QueryClientProvider>,
  );
}

describe('BindingsEditorDialog', () => {
  afterEach(() => {
    resetGeneratedClient();
  });

  it('saves a binding with the same step the wizard uses, and says it was saved', async () => {
    mock(STATE);
    const user = userEvent.setup();
    const dialog = await screen.findByTestId('bindings-editor');

    await user.click(await within(dialog).findByRole('combobox', { name: 'github for Release bot' }));
    await user.click(await screen.findByRole('option', { name: 'GitHub (release bot)' }));
    await user.click(within(dialog).getByRole('button', { name: 'Save bindings' }));

    await waitFor(() =>
      expect(puts).toEqual([{ bindings: [{ requirement_key: 'toolkit:github', toolkit_id: '12' }] }]),
    );
    expect(await within(dialog).findByTestId('bindings-saved')).toBeInTheDocument();
  });

  it('starts from what the server holds and can disable a toolkit for now', async () => {
    mock({ ...STATE, bindings: [{ requirement_key: 'toolkit:github', toolkit_id: '12' }] });
    const user = userEvent.setup();
    const dialog = await screen.findByTestId('bindings-editor');

    const select = await within(dialog).findByRole('combobox', { name: 'github for Release bot' });
    await waitFor(() => expect(select).toHaveTextContent('GitHub (release bot)'));
    await user.click(select);
    await user.click(await screen.findByRole('option', { name: 'Disable for now' }));
    await user.click(within(dialog).getByRole('button', { name: 'Save bindings' }));

    await waitFor(() => expect(puts).toEqual([{ bindings: [{ requirement_key: 'toolkit:github', disabled: true }] }]));
  });

  it('cannot save while a required toolkit is unbound, and shows the server refusal when it answers binding_missing', async () => {
    mock(STATE, () => HttpResponse.json({ error: 'binding_missing' }, { status: 409 }));
    const user = userEvent.setup();
    const dialog = await screen.findByTestId('bindings-editor');
    await within(dialog).findByRole('combobox', { name: 'github for Release bot' });
    expect(within(dialog).getByRole('button', { name: 'Save bindings' })).toBeDisabled();

    await user.click(within(dialog).getByRole('combobox', { name: 'github for Release bot' }));
    await user.click(await screen.findByRole('option', { name: 'GitHub (release bot)' }));
    await user.click(within(dialog).getByRole('button', { name: 'Save bindings' }));

    expect(await within(dialog).findByTestId('bindings-error')).toHaveTextContent('Bind every required toolkit');
  });

  it('says the tools stay off while the hire waits for approval', async () => {
    mock({ ...STATE, awaiting_approval: true });
    const dialog = await screen.findByTestId('bindings-editor');
    expect(await within(dialog).findByTestId('bindings-awaiting')).toHaveTextContent(
      'The tools stay off until a person approves the hire.',
    );
  });
});
