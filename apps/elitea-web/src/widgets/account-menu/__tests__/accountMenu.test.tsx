/**
 * The account menu at the sidebar's foot: the person on the button, the
 * moved Elitea items, the language choice, the version line, the admin
 * portal gate, and the desktop's bug report.
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

import { AccountMenu, type AccountMenuItem } from '../ui/AccountMenu';

const globals = globalThis as unknown as Record<string, unknown>;
const ITEMS: AccountMenuItem[] = [
  { value: 'credentials', label: 'Credentials', url: '/credentials' },
  { value: 'catalog', label: 'Catalog', url: '/elitea-catalog' },
  { value: 'help', label: 'Help Center', url: '/help-center' },
];

function mount(platform: 'web' | 'desktop' = 'desktop', collapsed = false) {
  const go = vi.fn<(to: string) => void>();
  const reload = vi.fn<() => void>();
  render(
    <AppProviders>
      <AccountMenu items={ITEMS} go={go} platform={platform} collapsed={collapsed} reload={reload} />
    </AppProviders>,
  );
  return { go, reload };
}

function administration(names: string[]) {
  server.use(http.get('*/auth/permissions/administration', () => HttpResponse.json(names.map((name) => ({ name, enabled: true })))));
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: '1' };
  configureGeneratedClient({ baseUrl: '/api/v2' });
  clearNamespace();
  server.use(http.get('*/social/author', () => HttpResponse.json({ id: 3, name: 'Alex K', email: 'alex@example.com', avatar: '' })));
  administration([]);
  server.use(http.get('*/.well-known/elitea-client', () => HttpResponse.json({ server_version: '1.72' })));
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
    const { go } = mount();
    const button = screen.getByTestId('shell-account');
    await waitFor(() => expect(button).toHaveTextContent('Alex K'));
    await user.click(button);
    const menu = await screen.findByRole('menu', { name: 'Account menu' });
    expect(within(menu).getByText('alex@example.com')).toBeInTheDocument();
    expect(within(menu).getByRole('menuitem', { name: 'Credentials' })).toBeInTheDocument();
    expect(within(menu).getByRole('menuitem', { name: 'Get help' })).toBeInTheDocument();
    expect(within(menu).getByRole('menuitem', { name: 'Sign out' })).toBeInTheDocument();
    await user.click(within(menu).getByRole('menuitem', { name: 'Catalog' }));
    expect(go).toHaveBeenCalledWith('/elitea-catalog');
  });

  it('names an account that has only an email by its email', async () => {
    server.use(http.get('*/social/author', () => HttpResponse.json({ id: 4, name: null, email: 'sso@example.com' })));
    mount();
    await waitFor(() => expect(screen.getByTestId('shell-account')).toHaveTextContent('sso@example.com'));
  });

  it('saves a new language and reloads to apply it', async () => {
    const user = userEvent.setup();
    const { reload } = mount();
    await user.click(screen.getByTestId('shell-account'));
    await user.click(await screen.findByTestId('shell-account-language'));
    const languages = await screen.findByRole('menu', { name: 'Language' });
    expect(within(languages).getByRole('menuitemradio', { name: 'English' })).toBeChecked();
    expect(within(languages).getByRole('menuitemradio', { name: 'Español' })).not.toBeChecked();
    await user.click(within(languages).getByRole('menuitemradio', { name: 'Español' }));
    expect(createStorage('local').get('lang')).toBe('es');
    expect(reload).toHaveBeenCalledOnce();
  });

  it('does not reload when the current language is chosen again', async () => {
    const user = userEvent.setup();
    const { reload } = mount();
    await user.click(screen.getByTestId('shell-account'));
    await user.click(await screen.findByTestId('shell-account-language'));
    await user.click(within(await screen.findByRole('menu', { name: 'Language' })).getByRole('menuitemradio', { name: 'English' }));
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

  it('shows the web build version and the server version, and copies them', async () => {
    const user = userEvent.setup();
    const copy = vi.fn<(text: string) => Promise<void>>().mockResolvedValue(undefined);
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: copy }, configurable: true });
    server.use(http.get('*/.well-known/elitea-client', () => HttpResponse.json({ server_version: '1.72' })));
    mount('web');
    await user.click(screen.getByTestId('shell-account'));
    const row = await screen.findByTestId('account-menu-version');
    await waitFor(() => expect(row).toHaveTextContent('Version dev · Server 1.72'));
    await user.click(row);
    expect(copy).toHaveBeenCalledWith('Version dev · Server 1.72');
    await waitFor(() => expect(row).toHaveTextContent('Copied'));
  });

  it('leaves the server out when the discovery document does not answer', async () => {
    const user = userEvent.setup();
    server.use(http.get('*/.well-known/elitea-client', () => new HttpResponse(null, { status: 503 })));
    mount('web');
    await user.click(screen.getByTestId('shell-account'));
    const row = await screen.findByTestId('account-menu-version');
    expect(row).toHaveTextContent('Version dev');
    expect(row).not.toHaveTextContent('Server');
  });

  it('offers no admin portal to a person without an administration permission', async () => {
    const user = userEvent.setup();
    mount('web');
    await user.click(screen.getByTestId('shell-account'));
    const menu = await screen.findByRole('menu', { name: 'Account menu' });
    await within(menu).findByRole('menuitem', { name: 'Settings' });
    expect(within(menu).queryByRole('menuitem', { name: /Admin portal/ })).not.toBeInTheDocument();
    expect(screen.queryByTestId('shell-account-admin-portal')).not.toBeInTheDocument();
  });

  it('links an administrator to the admin portal on this origin (web)', async () => {
    administration(['admin.users.view']);
    const user = userEvent.setup();
    mount('web');
    await user.click(screen.getByTestId('shell-account'));
    const portal = await screen.findByTestId('shell-account-admin-portal');
    expect(portal).toHaveAttribute('href', '/admin/app/');
    expect(portal).toHaveTextContent('Admin portal');
  });

  it('opens the admin portal in the browser on the desktop', async () => {
    administration(['admin.users.view']);
    const open = vi.spyOn(window, 'open').mockReturnValue(null);
    const user = userEvent.setup();
    mount('desktop');
    await user.click(screen.getByTestId('shell-account'));
    const portal = await screen.findByTestId('shell-account-admin-portal');
    expect(portal).not.toHaveAttribute('href');
    await user.click(portal);
    expect(open).toHaveBeenCalledWith('/admin/app/', '_blank', 'noopener');
  });

  describe('IT support line (ADR-0052 W6.2)', () => {
    function host(state: Record<string, unknown>) {
      globals['__TAURI_INTERNALS__'] = {
        invoke: (command: string) =>
          command === 'host_state'
            ? Promise.resolve({ configured: true, origin: 'https://elitea.example', displayName: 'Acme', signedIn: true, clientVersion: '1.0.0', clientId: 'x', policy: null, ...state })
            : Promise.reject(new Error(command)),
      };
    }
    afterEach(() => {
      delete globals['__TAURI_INTERNALS__'];
    });

    it('shows the managed line over the workspace one, and opens an https line in the browser', async () => {
      host({ managedSupportContact: 'Managed desk https://help.example.com/it', policy: { support_contact: 'Workspace desk' } });
      const open = vi.spyOn(window, 'open').mockReturnValue(null);
      const user = userEvent.setup();
      mount('desktop');
      await user.click(screen.getByTestId('shell-account'));
      const row = await screen.findByTestId('shell-account-it-support');
      expect(row).toHaveTextContent('IT support');
      expect(within(row).getByTitle('Managed desk https://help.example.com/it')).toHaveTextContent('Managed desk https://help.examp…');
      await user.click(row);
      expect(open).toHaveBeenCalledWith('https://help.example.com/it', '_blank', 'noopener');
    });

    it('a workspace email line goes to Troubleshoot, where it can be copied', async () => {
      host({ policy: { support_contact: 'IT desk, help@example.com' } });
      const open = vi.spyOn(window, 'open').mockReturnValue(null);
      const user = userEvent.setup();
      const { go } = mount('desktop');
      await user.click(screen.getByTestId('shell-account'));
      await user.click(await screen.findByTestId('shell-account-it-support'));
      expect(go).toHaveBeenCalledWith('/settings/troubleshoot');
      expect(open).not.toHaveBeenCalled();
    });

    it('no line, no row; and never on the web', async () => {
      host({});
      const user = userEvent.setup();
      mount('desktop');
      await user.click(screen.getByTestId('shell-account'));
      await screen.findByRole('menu', { name: 'Account menu' });
      expect(screen.queryByTestId('shell-account-it-support')).toBeNull();
    });
  });

  it('collapsed (web rail): the avatar alone opens the same menu, with no bug button', async () => {
    const user = userEvent.setup();
    mount('web', true);
    const button = screen.getByTestId('shell-account');
    await waitFor(() => expect(button).toHaveTextContent('AK'));
    // Only the avatar shows; the name is there for assistive tech alone, and
    // the button is named by its content (axe label-content-name-mismatch).
    expect(button).not.toHaveAttribute('aria-label');
    expect(button).toHaveAccessibleName('Alex K, Account menu');
    expect(within(button).getByText('Alex K')).toHaveStyle({ position: 'absolute', overflow: 'hidden' });
    expect(screen.queryByTestId('shell-report-bug')).not.toBeInTheDocument();
    await user.click(button);
    expect(await screen.findByRole('menu', { name: 'Account menu' })).toBeInTheDocument();

    // A mouse press opens it without focusing the button; Escape must still
    // hand focus back to the button, not drop it on <body>.
    await user.keyboard('{Escape}');
    await waitFor(() => expect(screen.queryByRole('menu')).not.toBeInTheDocument());
    await waitFor(() => expect(button).toHaveFocus());
  });
});
