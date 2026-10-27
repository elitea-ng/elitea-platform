/**
 * The account menu at the sidebar's foot: the person and tenant on the
 * button, the moved Elitea items, the language choice and the bug report.
 */
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { createStorage, clearNamespace } from '@/shared/lib/storage';

import { server } from '../../../test/setup';

import type { EliteaItem } from '../lib/eliteaSections';
import type { ShellAction } from '../lib/shellActions';
import { AccountMenu } from '../ui/AccountMenu';

const globals = globalThis as unknown as Record<string, unknown>;
const ITEMS: EliteaItem[] = [
  { value: 'credentials', label: 'Credentials', url: '/credentials' },
  { value: 'catalog', label: 'Catalog', url: '/elitea-catalog' },
  { value: 'help', label: 'Help Center', url: '/help-center' },
];

function mount() {
  const run = vi.fn<(action: ShellAction) => void>();
  const reload = vi.fn<() => void>();
  render(
    <AppProviders>
      <AccountMenu items={ITEMS} run={run} reload={reload} />
    </AppProviders>,
  );
  return { run, reload };
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: '/api/v2' });
  clearNamespace();
  server.use(http.get('*/social/author', () => HttpResponse.json({ id: 3, name: 'Alex K', email: 'alex@example.com', avatar: '' })));
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  delete globals['elitea_ui_config'];
  vi.restoreAllMocks();
});

describe('AccountMenu', () => {
  it('names the person on the button and opens the account items from the menu', async () => {
    const user = userEvent.setup();
    const { run } = mount();
    const button = screen.getByTestId('shell-account');
    await waitFor(() => expect(button).toHaveTextContent('Alex K'));
    await user.click(button);
    const menu = await screen.findByRole('menu', { name: 'Account menu' });
    expect(within(menu).getByText('alex@example.com')).toBeInTheDocument();
    expect(within(menu).getByRole('menuitem', { name: 'Credentials' })).toBeInTheDocument();
    expect(within(menu).getByRole('menuitem', { name: 'Get help' })).toBeInTheDocument();
    expect(within(menu).getByRole('menuitem', { name: 'Sign out' })).toBeInTheDocument();
    await user.click(within(menu).getByRole('menuitem', { name: 'Catalog' }));
    expect(run).toHaveBeenCalledWith({ type: 'go', to: '/elitea-catalog' });
  });

  it('saves a new language and reloads to apply it', async () => {
    const user = userEvent.setup();
    const { reload } = mount();
    await user.click(screen.getByTestId('shell-account'));
    await user.click(await screen.findByTestId('shell-account-language'));
    const languages = await screen.findByRole('menu', { name: 'Language' });
    expect(within(languages).getByRole('menuitem', { name: 'English' })).toHaveClass('Mui-selected');
    await user.click(within(languages).getByRole('menuitem', { name: 'Español' }));
    expect(createStorage('local').get('lang')).toBe('es');
    expect(reload).toHaveBeenCalledOnce();
  });

  it('does not reload when the current language is chosen again', async () => {
    const user = userEvent.setup();
    const { reload } = mount();
    await user.click(screen.getByTestId('shell-account'));
    await user.click(await screen.findByTestId('shell-account-language'));
    await user.click(within(await screen.findByRole('menu', { name: 'Language' })).getByRole('menuitem', { name: 'English' }));
    expect(reload).not.toHaveBeenCalled();
    expect(createStorage('local').get('lang')).toBeNull();
  });

  it('opens a prefilled bug report in the browser', async () => {
    const user = userEvent.setup();
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    mount();
    await user.click(screen.getByRole('button', { name: 'Report a bug' }));
    expect(open).toHaveBeenCalledOnce();
    expect(String(open.mock.calls[0]?.[0])).toMatch(/^https:\/\/github\.com\/elitea-ng\/elitea-platform\/issues\/new\?/);
  });
});
