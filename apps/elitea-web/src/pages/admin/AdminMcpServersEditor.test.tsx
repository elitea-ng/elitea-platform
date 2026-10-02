/**
 * Rendering + write-path guard for the pre-built MCP catalogue editor.
 *
 * The properties asserted here are the ones this screen's hazards make worth
 * asserting, and each one is invisible to a status-code test:
 *
 *  1. **The tri-state `client_secret`.** Absent, `''` and a value mean leave it,
 *     clear it, and re-seal it. The dialog cannot echo the stored secret, so a
 *     save that always sent the field would destroy the credential every time
 *     an operator edited a URL. This is the one bug on this screen that is
 *     silent, permanent and impossible to notice from the UI, so the BODY of
 *     every write is inspected, not just its status.
 *  2. **No plaintext secret is ever rendered.** The listing carries a mask and
 *     there is no reveal, because nothing can reveal it.
 *  3. **Every request goes to the `administration` mode**, which is the only
 *     scope the catalogue has.
 *  4. **The derived catalogue key matches the server's rule**, including its
 *     odd strip-after-replace ordering. The key — not the display name — is
 *     what a toolkit type resolves against at run time.
 *  5. **A refusal renders the SERVER's own sentence**, which on this surface is
 *     specific and actionable ("only http MCP servers can be catalogued here…").
 *
 * No fixture value here is or resembles a real credential.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import { QueryClient } from '@tanstack/react-query';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { getListToolkitsQueryKey } from '@/shared/api/generated/toolkits/toolkits';
import { server } from '@/test/setup';

import { AdminMcpServersEditor } from './AdminMcpServersEditor';
import { normalizeCatalogueKey, resolveSecretForSave } from './adminMcpServerForm';
import { renderAdminRoute } from './__tests__/testRouter';

const CATALOGUE = [
  {
    key: 'github_copilot',
    display_name: 'GitHub Copilot',
    url: 'https://api.githubcopilot.com/mcp/',
    base_url: '',
    client_id: 'copilot-client',
    client_secret: '******',
    timeout: 30,
    headers: { Authorization: 'Bearer {api_token}' },
    config_schema: {
      properties: {
        api_token: { type: 'string', required: true, secret: true },
      },
    },
    enabled: true,
  },
  {
    key: 'withdrawn_thing',
    display_name: 'Withdrawn Thing',
    url: 'https://withdrawn.example.com/mcp',
    base_url: '',
    client_id: '',
    timeout: 0,
    headers: {},
    config_schema: { properties: {} },
    enabled: false,
  },
];

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

function useCatalogueHandlers(
  options: { saveStatus?: number; saveBody?: Record<string, string> } = {},
): void {
  server.use(
    http.get('*/admin/mcp_prebuilt_servers/administration', ({ request }) => {
      recorded.push({ method: 'GET', url: request.url, body: null });
      return HttpResponse.json({ servers: CATALOGUE, total: CATALOGUE.length });
    }),
    http.put('*/admin/mcp_prebuilt_servers/administration/*', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      if (options.saveStatus !== undefined) {
        return HttpResponse.json(options.saveBody, { status: options.saveStatus });
      }
      return HttpResponse.json({ key: 'github_copilot', display_name: 'GitHub Copilot' });
    }),
    http.delete('*/admin/mcp_prebuilt_servers/administration/*', ({ request }) => {
      recorded.push({ method: 'DELETE', url: request.url, body: null });
      return HttpResponse.json({ deleted: 'github_copilot' });
    }),
  );
}

function writes(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method !== 'GET');
}

function catalogueCacheClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: Infinity },
      mutations: { retry: false },
    },
  });
}

beforeEach(() => {
  recorded = [];
  configureGeneratedClient({ baseUrl: '/api/v2' });
  useCatalogueHandlers();
});

afterEach(() => {
  resetGeneratedClient();
});

async function openEditFor(name: string): Promise<void> {
  const user = userEvent.setup();
  await screen.findByText(name);
  const row = screen.getByText(name).closest('tr');
  if (!row) throw new Error(`no row for ${name}`);
  const edit = row.querySelector('button');
  // The first button in the actions cell is Edit; find it by name to avoid
  // depending on order.
  const buttons = Array.from(row.querySelectorAll('button'));
  const editButton = buttons.find((button) => button.textContent === 'Edit') ?? edit;
  if (!editButton) throw new Error(`no edit button for ${name}`);
  await user.click(editButton);
}

describe('Admin › MCP Servers catalogue', () => {
  it('lists catalogued servers with their key, and marks a withdrawn one', async () => {
    renderAdminRoute(<AdminMcpServersEditor />);

    expect(await screen.findByText('GitHub Copilot')).toBeInTheDocument();
    // The KEY is shown, not only the display name: it is what a toolkit type
    // resolves against, so an operator debugging a toolkit needs to see it.
    expect(screen.getByText('github_copilot')).toBeInTheDocument();
    expect(screen.getByText('Withdrawn Thing')).toBeInTheDocument();
    expect(screen.getByText('Offered')).toBeInTheDocument();
    expect(screen.getByText('Withdrawn')).toBeInTheDocument();
  });

  it('renders the mask for a stored secret and never a plaintext value', async () => {
    renderAdminRoute(<AdminMcpServersEditor />);

    expect(await screen.findByText('******')).toBeInTheDocument();
    // The entry with no secret says so rather than rendering an empty cell,
    // so "no secret" and "a secret you may not read" stay distinguishable.
    expect(screen.getByText('None')).toBeInTheDocument();
    // There is no reveal control, because there is nothing to reveal.
    expect(screen.queryByRole('button', { name: /reveal|show/i })).not.toBeInTheDocument();
  });

  it('reads the catalogue from the administration mode', async () => {
    renderAdminRoute(<AdminMcpServersEditor />);
    await screen.findByText('GitHub Copilot');

    expect(recorded[0]?.url).toContain('/admin/mcp_prebuilt_servers/administration');
  });

  it('OMITS client_secret when the operator did not touch it', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminMcpServersEditor />);
    await openEditFor('GitHub Copilot');

    // Change only the URL — the case that happens constantly and must not
    // destroy the sealed credential.
    const url = await screen.findByLabelText('Server URL');
    await user.clear(url);
    await user.type(url, 'https://changed.example.com/mcp');
    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    const body = writes()[0]?.body as Record<string, unknown>;
    expect(body['url']).toBe('https://changed.example.com/mcp');
    expect(Object.hasOwn(body, 'client_secret')).toBe(false);
  });

  it('sends an EMPTY client_secret only when the operator asks to clear it', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminMcpServersEditor />);
    await openEditFor('GitHub Copilot');

    await user.click(await screen.findByTestId('admin-mcp-server-clear-secret'));
    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    const body = writes()[0]?.body as Record<string, unknown>;
    expect(body['client_secret']).toBe('');
  });

  it('sends a typed client_secret, and declares the http transport', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminMcpServersEditor />);
    await openEditFor('GitHub Copilot');

    await user.type(await screen.findByLabelText('OAuth client secret'), 'rotated-value');
    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    const body = writes()[0]?.body as Record<string, unknown>;
    expect(body['client_secret']).toBe('rotated-value');
    // Declared explicitly so the server's stdio refusal is a contract this
    // client is on the right side of, not a default it leans on.
    expect(body['transport']).toBe('http');
  });

  it('invalidates every project toolkit catalogue after a save', async () => {
    const user = userEvent.setup();
    const queryClient = catalogueCacheClient();
    renderAdminRoute(<AdminMcpServersEditor />, { queryClient });
    await openEditFor('GitHub Copilot');

    const privateCatalogue = getListToolkitsQueryKey('private-project');
    const publicCatalogue = getListToolkitsQueryKey('public-project');
    const unrelated = ['unrelated', 'query'] as const;
    queryClient.setQueryData(privateCatalogue, { data: {} });
    queryClient.setQueryData(publicCatalogue, { data: {} });
    queryClient.setQueryData(unrelated, 'keep');

    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(queryClient.getQueryState(privateCatalogue)?.isInvalidated).toBe(true);
      expect(queryClient.getQueryState(publicCatalogue)?.isInvalidated).toBe(true);
    });
    expect(queryClient.getQueryState(unrelated)?.isInvalidated).toBe(false);
  });

  it('edits templated headers and the dynamic project parameter schema', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminMcpServersEditor />);
    await openEditFor('GitHub Copilot');

    fireEvent.change(await screen.findByLabelText('Headers (YAML mapping)'), {
      target: {
        value: 'Authorization: Bearer {project_token}\nX-Tenant: "{tenant}"\n',
      },
    });
    fireEvent.change(screen.getByLabelText('Project parameter schema (YAML mapping)'), {
      target: {
        value:
          'properties:\n  project_token:\n    type: string\n    required: true\n    secret: true\n  tenant:\n    type: string\n    required: true\n',
      },
    });
    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    const body = writes()[0]?.body as Record<string, unknown>;
    expect(body['headers']).toEqual({
      Authorization: 'Bearer {project_token}',
      'X-Tenant': '{tenant}',
    });
    expect(body['config_schema']).toEqual({
      properties: {
        project_token: { type: 'string', required: true, secret: true },
        tenant: { type: 'string', required: true },
      },
    });
  });

  it('keeps an invalid YAML mapping in the dialog and sends no write', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminMcpServersEditor />);
    await openEditFor('GitHub Copilot');

    fireEvent.change(await screen.findByLabelText('Headers (YAML mapping)'), {
      target: { value: '- not\n- a\n- mapping\n' },
    });
    await user.click(screen.getByRole('button', { name: 'Save' }));

    expect(await screen.findByTestId('admin-mcp-server-dialog-error')).toHaveTextContent(
      'Headers must be a valid YAML mapping.',
    );
    expect(writes()).toHaveLength(0);
  });

  it('keeps the dialog open and renders the SERVER reason when a save is refused', async () => {
    const user = userEvent.setup();
    useCatalogueHandlers({
      saveStatus: 400,
      saveBody: { error: 'only http MCP servers can be catalogued here' },
    });
    renderAdminRoute(<AdminMcpServersEditor />);
    await openEditFor('GitHub Copilot');

    await user.click(screen.getByRole('button', { name: 'Save' }));

    expect(await screen.findByTestId('admin-mcp-server-dialog-error')).toHaveTextContent(
      'only http MCP servers can be catalogued here',
    );
    // The typed input survives the refusal: closing would discard the
    // operator's work along with the reason it was rejected.
    expect(screen.getByTestId('admin-mcp-server-dialog')).toBeInTheDocument();
  });

  it('confirms before a delete, and says the secret goes with it', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<AdminMcpServersEditor />);
    await screen.findByText('GitHub Copilot');

    const row = screen.getByText('GitHub Copilot').closest('tr');
    const remove = Array.from(row?.querySelectorAll('button') ?? []).find(
      (button) => button.textContent === 'Remove',
    );
    await user.click(remove as HTMLElement);

    // Nothing is sent until the confirmation is accepted.
    expect(writes()).toHaveLength(0);
    expect(await screen.findByTestId('admin-mcp-server-delete-dialog')).toHaveTextContent(
      /cannot be recovered/,
    );

    await user.click(screen.getByTestId('admin-mcp-server-delete-confirm'));
    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    expect(writes()[0]?.method).toBe('DELETE');
    expect(writes()[0]?.url).toContain('/administration/github_copilot');
  });

  it('invalidates every project toolkit catalogue after a delete', async () => {
    const user = userEvent.setup();
    const queryClient = catalogueCacheClient();
    renderAdminRoute(<AdminMcpServersEditor />, { queryClient });
    await screen.findByText('GitHub Copilot');

    const catalogue = getListToolkitsQueryKey('private-project');
    queryClient.setQueryData(catalogue, { data: {} });

    const row = screen.getByText('GitHub Copilot').closest('tr');
    const remove = Array.from(row?.querySelectorAll('button') ?? []).find(
      (button) => button.textContent === 'Remove',
    );
    await user.click(remove as HTMLElement);
    await user.click(screen.getByTestId('admin-mcp-server-delete-confirm'));

    await waitFor(() => {
      expect(queryClient.getQueryState(catalogue)?.isInvalidated).toBe(true);
    });
  });

  it('shows an empty catalogue as a statement, not as a blank pane', async () => {
    server.use(
      http.get('*/admin/mcp_prebuilt_servers/administration', () =>
        HttpResponse.json({ servers: [], total: 0 }),
      ),
    );
    renderAdminRoute(<AdminMcpServersEditor />);

    expect(await screen.findByTestId('admin-mcp-servers-empty')).toBeInTheDocument();
  });
});

/**
 * The key derivation must stay identical to
 * `internal/mcpregistry.NormalizeCatalogueKey`, ORDERING INCLUDED. If the two
 * drift, the key this dialog shows is not the key the entry is stored under and
 * a toolkit's run-time lookup misses.
 */
describe('normalizeCatalogueKey', () => {
  it('matches the server rule, including its strip-after-replace order', () => {
    expect(normalizeCatalogueKey('mcp_epam_presales')).toBe('epam_presales');
    expect(normalizeCatalogueKey('Epam Presales')).toBe('epam_presales');
    expect(normalizeCatalogueKey('GitHub Copilot')).toBe('github_copilot');
    expect(normalizeCatalogueKey('MCP Epam Presales')).toBe('epam_presales');
    expect(normalizeCatalogueKey('mcp_mcp_thing')).toBe('mcp_thing');
    expect(normalizeCatalogueKey('')).toBe('');
    // Padding survives as underscores. Python runs `.lower().replace(" ","_")`
    // BEFORE `.strip()`, so by the time strip runs there is no whitespace left.
    // Both stacks derive the same odd key, which is the only property that
    // matters — "correcting" it here would break the join.
    expect(normalizeCatalogueKey('  Epam Presales  ')).toBe('__epam_presales__');
  });
});

/** The three-line function where a wrong answer silently destroys a credential. */
describe('resolveSecretForSave', () => {
  it('maps the two controls onto the server tri-state', () => {
    expect(resolveSecretForSave('', false)).toBeUndefined();
    expect(resolveSecretForSave('', true)).toBe('');
    expect(resolveSecretForSave('typed', false)).toBe('typed');
    // Clearing WINS over a typed value: the checkbox disables the field, so a
    // stale keystroke must not resurrect a secret the operator chose to remove.
    expect(resolveSecretForSave('typed', true)).toBe('');
  });
});
