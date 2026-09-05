/**
 * The platform Providers tab.
 *
 * What is pinned here is the handling of credentials, because every one of
 * these can be wrong while the screen looks right:
 *
 *  1. **A secret is never pre-filled and an untouched one is never sent.** The
 *     server does not return secret material, so a blank field on an edit means
 *     "keep the stored key" — and sending an empty string instead would erase a
 *     working credential across every project at once.
 *  2. **`status_ok = false` is surfaced as its own state.** The gateway admits
 *     nothing else, so such a credential is stored, listed and completely inert;
 *     a table that showed it like any other row would be reporting a provider
 *     that serves no request.
 *  3. **An unsealed secret is reported as a finding.** It is readable by every
 *     holder of the project-scoped configuration permissions on the public
 *     project, and re-saving is the fix.
 *  4. **An empty list is not shown when the read failed.** "No platform
 *     providers yet" is the reading that makes an operator publish a duplicate.
 *  5. **A scope mismatch with the gateway is reported.** elitea-main publishes
 *     into one project and the gateway resolves out of another; when they
 *     disagree the credential is stored, listed and healthy, and resolves for
 *     nobody. That is the only failure on this screen with no other symptom.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { LlmProxyProvidersPanel } from './LlmProxyProvidersPanel';
import { renderAdminRoute } from './__tests__/testRouter';

/** What the shipped catalogue admits — all nine the gateway dispatches to. */
const PROVIDER_TYPES = [
  'open_ai',
  'azure_open_ai',
  'open_ai_azure',
  'ai_dial',
  'anthropic',
  'ollama',
  'amazon_bedrock',
  'vertex_ai',
  'vllm',
];

/** A deployment whose catalogue is narrower than this build's form model. */
const NARROWED_PROVIDER_TYPES = ['open_ai', 'azure_open_ai', 'ai_dial', 'amazon_bedrock', 'vertex_ai', 'ollama'];

const SEALED_OPENAI = {
  id: 4,
  uuid: 'uuid-4',
  elitea_title: 'platform-openai',
  label: '',
  type: 'open_ai',
  status_ok: true,
  status_logs: '',
  endpoint: 'https://api.openai.com/v1',
  settings: {},
  secrets: [{ field: 'api_key', set: true, sealed: true }],
  created_at: '2026-08-01T00:00:00Z',
  updated_at: '2026-08-01T00:00:00Z',
};

function useProviders(items: unknown[], publicProjectID = 1, providerTypes: readonly string[] = PROVIDER_TYPES): void {
  server.use(
    http.get('*/admin/gateway/providers', () =>
      HttpResponse.json({
        items,
        total: items.length,
        public_project_id: publicProjectID,
        provider_types: providerTypes,
      }),
    ),
  );
}

/** What the gateway reports about its own shared scope. */
function useGatewayScope(sharedProjectID: string | undefined): void {
  server.use(
    http.get('*/admin/gateway/status', () =>
      HttpResponse.json({
        reachable: true,
        gateway:
          sharedProjectID === undefined
            ? { enabled: true }
            : { enabled: true, shared_project_id: sharedProjectID },
      }),
    ),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('LlmProxyProvidersPanel', () => {
  it('lists the platform providers and names the shared project', async () => {
    useProviders([SEALED_OPENAI], 7);
    renderAdminRoute(<LlmProxyProvidersPanel />);

    expect(await screen.findByTestId('llm-providers-table')).toHaveTextContent('platform-openai');
    expect(screen.getByTestId('llm-providers-table')).toHaveTextContent('OpenAI');
    // Which project is the shared one is echoed: getting it wrong is the
    // failure where every credential is published into a schema the gateway
    // never reads, and everything else on the screen still looks right.
    expect(screen.getByText(/Shared project: 7/)).toBeVisible();
  });

  it('marks a credential the gateway will not admit', async () => {
    useProviders([{ ...SEALED_OPENAI, status_ok: false }]);
    renderAdminRoute(<LlmProxyProvidersPanel />);

    // Not "inactive" — nobody switched it off. It did not resolve, and the
    // action is to correct the endpoint or the key.
    expect(await screen.findByText('Not resolving')).toBeVisible();
  });

  it('reports a secret that was never sealed into the vault', async () => {
    useProviders([
      { ...SEALED_OPENAI, secrets: [{ field: 'api_key', set: true, sealed: false }] },
    ]);
    renderAdminRoute(<LlmProxyProvidersPanel />);

    expect(await screen.findByTestId('llm-providers-unsealed')).toHaveTextContent('platform-openai');
  });

  it('does not claim there are no providers when the read failed', async () => {
    server.use(
      http.get('*/admin/gateway/providers', () =>
        HttpResponse.json({ error: 'access_denied' }, { status: 403 }),
      ),
    );
    renderAdminRoute(<LlmProxyProvidersPanel />);

    expect(await screen.findByTestId('llm-providers-load-error')).toBeVisible();
    // The empty state would send an operator to publish a duplicate of a
    // credential that already exists.
    expect(screen.queryByTestId('llm-providers-empty')).toBeNull();
  });

  it('says an empty platform has none, and what that means', async () => {
    useProviders([]);
    renderAdminRoute(<LlmProxyProvidersPanel />);

    expect(await screen.findByTestId('llm-providers-empty')).toHaveTextContent(
      'every project must configure its own',
    );
  });

  // The core credential-handling assertion.
  it('omits an untouched secret on an edit rather than clearing it', async () => {
    useProviders([SEALED_OPENAI]);
    const bodies: unknown[] = [];
    server.use(
      http.put('*/admin/gateway/providers/4', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ id: 4 });
      }),
    );
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }));

    // The stored key is not on screen, because the server never sent it.
    const apiKey = await screen.findByTestId('llm-provider-api_key');
    expect(apiKey).toHaveValue('');

    const name = screen.getByTestId('llm-provider-name');
    await userEvent.clear(name);
    await userEvent.type(name, 'renamed-openai');
    await userEvent.click(screen.getByTestId('llm-provider-save'));

    await waitFor(() => {
      expect(bodies).toHaveLength(1);
    });
    const sent = bodies[0] as { elitea_title: string; data: Record<string, unknown> };
    expect(sent.elitea_title).toBe('renamed-openai');
    // An empty string here would erase the key for every project on the
    // platform, and the save would report success.
    expect(Object.hasOwn(sent.data, 'api_key')).toBe(false);
    expect(sent.data.api_base).toBe('https://api.openai.com/v1');
  });

  it('sends a new secret when the operator entered one', async () => {
    useProviders([SEALED_OPENAI]);
    const bodies: unknown[] = [];
    server.use(
      http.put('*/admin/gateway/providers/4', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ id: 4 });
      }),
    );
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }));
    await userEvent.type(await screen.findByTestId('llm-provider-api_key'), 'sk-rotated');
    await userEvent.click(screen.getByTestId('llm-provider-save'));

    await waitFor(() => {
      expect(bodies).toHaveLength(1);
    });
    expect((bodies[0] as { data: Record<string, unknown> }).data.api_key).toBe('sk-rotated');
  });

  it("never sends `shared`, because the server owns it", async () => {
    useProviders([]);
    const bodies: unknown[] = [];
    server.use(
      http.post('*/admin/gateway/providers', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ id: 9 }, { status: 201 });
      }),
    );
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await userEvent.click(await screen.findByTestId('llm-providers-add'));
    await userEvent.type(await screen.findByTestId('llm-provider-name'), 'new-openai');
    await userEvent.type(screen.getByTestId('llm-provider-api_key'), 'sk-new');
    await userEvent.click(screen.getByTestId('llm-provider-save'));

    await waitFor(() => {
      expect(bodies).toHaveLength(1);
    });
    // The server forces it and REFUSES an explicit false, so a client that sent
    // the field would either restate the rule or be refused for contradicting it.
    expect(Object.hasOwn(bodies[0] as object, 'shared')).toBe(false);
  });

  it("renders the server's own refusal rather than a generic failure", async () => {
    useProviders([]);
    server.use(
      http.post('*/admin/gateway/providers', () =>
        HttpResponse.json(
          { error: 'not an LLM provider credential type the gateway can dispatch to' },
          { status: 400 },
        ),
      ),
    );
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await userEvent.click(await screen.findByTestId('llm-providers-add'));
    await userEvent.type(await screen.findByTestId('llm-provider-name'), 'x');
    await userEvent.type(screen.getByTestId('llm-provider-api_key'), 'sk-x');
    await userEvent.click(screen.getByTestId('llm-provider-save'));

    expect(await screen.findByTestId('llm-provider-dialog-error')).toHaveTextContent(
      'the gateway can dispatch to',
    );
  });

  it('warns that deleting withdraws the credential from every project', async () => {
    useProviders([SEALED_OPENAI]);
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await userEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    expect(await screen.findByTestId('llm-providers-confirm-delete')).toHaveTextContent(
      'every project at once',
    );
    // Deletion needs a second, explicit act.
    expect(screen.getByTestId('llm-providers-confirm-delete-button')).toBeVisible();
  });
});


describe('LlmProxyProvidersPanel — the live check and re-validate actions', () => {
  // The STORED status_ok chip ("In use") never moves for a check — only a
  // Re-validate writes anything server-side. This pins that a proven round
  // trip renders its OWN, separate verdict rather than mutating the chip.
  it('renders a tick beside the stored status on a proven round trip', async () => {
    useProviders([SEALED_OPENAI]);
    server.use(
      http.post('*/admin/gateway/providers/4/check', () =>
        HttpResponse.json({ success: true, message: 'Connection successful' }),
      ),
    );
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await userEvent.click(await screen.findByTestId('admin-provider-check-4'));

    expect(await screen.findByTestId('admin-provider-check-result-4')).toHaveTextContent(
      'Live check: connected',
    );
    // The stored chip is untouched: a check writes nothing.
    expect(screen.getByText('In use')).toBeVisible();
  });

  // check_stored_connection answers a real failure as HTTP 400
  // {"success":false,"message":...} — the SAME shape its own 404 uses (see
  // the next test). This is the ordinary failure path.
  it('renders the failure message inline rather than a generic one', async () => {
    useProviders([SEALED_OPENAI]);
    server.use(
      http.post('*/admin/gateway/providers/4/check', () =>
        HttpResponse.json(
          { success: false, message: 'Could not verify the connection right now. Please try again.' },
          { status: 400 },
        ),
      ),
    );
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await userEvent.click(await screen.findByTestId('admin-provider-check-4'));

    expect(await screen.findByTestId('admin-provider-check-result-4')).toHaveTextContent(
      'Could not verify the connection right now',
    );
  });

  // THE ENVELOPE TRAP: `eliteaFetch` throws for every non-2xx response,
  // including the check route's OWN 404 ("Configuration not found" — a
  // configID this admin surface's public project does not hold). That 404
  // still carries `{"success":false,"message":...}`, the route's real,
  // renderable answer — not a transport failure. `useCheckAdminLlmProvider`
  // must catch exactly that shape and resolve with it: if this regresses to
  // a bare `eliteaFetch` call, the promise rejects, react-query flips to
  // `isError`, and the panel would either render nothing for this row or
  // need a second error path — never the row's own message.
  it("renders the check's own 404 message rather than throwing", async () => {
    useProviders([SEALED_OPENAI]);
    server.use(
      http.post('*/admin/gateway/providers/4/check', () =>
        HttpResponse.json({ success: false, message: 'Configuration not found' }, { status: 404 }),
      ),
    );
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await userEvent.click(await screen.findByTestId('admin-provider-check-4'));

    expect(await screen.findByTestId('admin-provider-check-result-4')).toHaveTextContent(
      'Configuration not found',
    );
    // A thrown, unhandled rejection would have left the mutation `isError`
    // with nothing rendered for this row, or torn down the table entirely —
    // it is still here, and no top-level alert fired for what is a per-row
    // result.
    expect(screen.getByTestId('llm-providers-table')).toBeVisible();
    expect(screen.queryByTestId('llm-providers-load-error')).toBeNull();
  });

  it("patches the row's status_ok from Re-validate's response, without a second read", async () => {
    let requests = 0;
    server.use(
      http.get('*/admin/gateway/providers', () => {
        requests += 1;
        return HttpResponse.json({
          items: [{ ...SEALED_OPENAI, status_ok: false }],
          total: 1,
          public_project_id: 1,
          provider_types: PROVIDER_TYPES,
        });
      }),
      http.post('*/admin/gateway/providers/4/revalidate', () =>
        HttpResponse.json({
          id: 4,
          project_id: '1',
          name: 'platform-openai',
          type: 'open_ai',
          section: 'ai_credentials',
          shared: true,
          status_ok: true,
          status_logs: '',
          source: 'admin',
          created_at: '2026-08-01T00:00:00Z',
          updated_at: '2026-08-29T00:00:00Z',
        }),
      ),
    );
    renderAdminRoute(<LlmProxyProvidersPanel />);

    expect(await screen.findByText('Not resolving')).toBeVisible();
    await userEvent.click(screen.getByTestId('admin-provider-revalidate-4'));

    expect(await screen.findByText('In use')).toBeVisible();
    // The cache was PATCHED from the response, not invalidated — a second
    // GET here would mean the row's endpoint/settings/secrets briefly
    // vanish (revalidate's response carries none of them) before the refetch
    // lands.
    expect(requests).toBe(1);
  });

  it("surfaces revalidate's own refusal rather than a generic failure", async () => {
    useProviders([SEALED_OPENAI]);
    server.use(
      http.post('*/admin/gateway/providers/4/revalidate', () =>
        HttpResponse.json({ error: 'configuration not found' }, { status: 404 }),
      ),
    );
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await userEvent.click(await screen.findByTestId('admin-provider-revalidate-4'));

    expect(await screen.findByTestId('llm-providers-revalidate-error')).toHaveTextContent(
      'configuration not found',
    );
    // The STORED status is unchanged: a failed revalidate must not be read
    // as "revalidated and found broken".
    expect(screen.getByText('In use')).toBeVisible();
  });
});


describe('LlmProxyProvidersPanel — the gateway shared scope', () => {
  it('warns when the gateway reads a different project', async () => {
    useProviders([SEALED_OPENAI], 1);
    useGatewayScope('7');
    renderAdminRoute(<LlmProxyProvidersPanel />);

    const warning = await screen.findByTestId('llm-providers-scope-mismatch');
    // Both numbers, because fixing it means making one match the other.
    expect(warning).toHaveTextContent('project 7');
    expect(warning).toHaveTextContent('project 1');
  });

  it('warns when the gateway is not reading the shared scope at all', async () => {
    useProviders([SEALED_OPENAI], 1);
    useGatewayScope('');
    renderAdminRoute(<LlmProxyProvidersPanel />);

    expect(await screen.findByTestId('llm-providers-scope-mismatch')).toHaveTextContent(
      'ELITEA_AI_PROJECT_ID',
    );
  });

  it('says nothing when the two agree', async () => {
    useProviders([SEALED_OPENAI], 1);
    useGatewayScope('1');
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await screen.findByTestId('llm-providers-table');
    expect(screen.queryByTestId('llm-providers-scope-mismatch')).toBeNull();
  });

  // `undefined` is NOT "off". A gateway too old to report the field, or one that
  // did not answer, must not raise an alarm about a mismatch nobody can see —
  // an unreachable gateway is already reported on the Status tab.
  it('says nothing when the gateway does not report the field', async () => {
    useProviders([SEALED_OPENAI], 1);
    useGatewayScope(undefined);
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await screen.findByTestId('llm-providers-table');
    expect(screen.queryByTestId('llm-providers-scope-mismatch')).toBeNull();
  });

  it('says nothing when the gateway did not answer', async () => {
    useProviders([SEALED_OPENAI], 1);
    server.use(
      http.get('*/admin/gateway/status', () =>
        HttpResponse.json({ reachable: false, error: 'dial tcp: connection refused' }),
      ),
    );
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await screen.findByTestId('llm-providers-table');
    expect(screen.queryByTestId('llm-providers-scope-mismatch')).toBeNull();
  });
});


describe('LlmProxyProvidersPanel — the offered provider types', () => {
  // The select must show what the SERVER admits, not what this app can draw.
  // A type outside the server's catalogue cannot be given a `section` or have
  // its key sealed. Offering one would produce a save the server refuses — or
  // worse, on a build whose check drifted, an inert row with a plaintext key.
  // A NARROWED server list is used, because the shipped catalogue describes all
  // nine: with the full list this case could not tell "follows the server" from
  // "draws everything it knows".
  it('offers only the types the server admits', async () => {
    useProviders([], 1, NARROWED_PROVIDER_TYPES);
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await userEvent.click(await screen.findByTestId('llm-providers-add'));
    await userEvent.click(await screen.findByRole('combobox'));

    expect(await screen.findByRole('option', { name: 'OpenAI' })).toBeVisible();
    // The three this server's catalogue does not describe.
    for (const absent of ['Anthropic', 'vLLM', 'Azure OpenAI (legacy naming)']) {
      expect(screen.queryByRole('option', { name: absent })).toBeNull();
    }
  });

  // The other direction: a server that DOES describe them offers all nine. The
  // shipped catalogue is that server, so an operator can publish a platform
  // credential for every provider the gateway dispatches to.
  it('offers every type a full catalogue admits', async () => {
    useProviders([]);
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await userEvent.click(await screen.findByTestId('llm-providers-add'));
    await userEvent.click(await screen.findByRole('combobox'));

    for (const present of ['OpenAI', 'Anthropic', 'vLLM', 'Azure OpenAI (legacy naming)']) {
      expect(await screen.findByRole('option', { name: present })).toBeVisible();
    }
  });

  it('drops a server-admitted type this build cannot draw a form for', async () => {
    server.use(
      http.get('*/admin/gateway/providers', () =>
        HttpResponse.json({
          items: [],
          total: 0,
          public_project_id: 1,
          // A type from a newer catalogue. Offering it would give the operator
          // an empty form and a credential saved with no key in it.
          provider_types: ['open_ai', 'some_future_provider'],
        }),
      ),
    );
    renderAdminRoute(<LlmProxyProvidersPanel />);

    await userEvent.click(await screen.findByTestId('llm-providers-add'));
    await userEvent.click(await screen.findByRole('combobox'));

    const options = await screen.findAllByRole('option');
    expect(options).toHaveLength(1);
    expect(options[0]).toHaveTextContent('OpenAI');
  });

  it('opens the adopt-models dialog for the row that was pressed', async () => {
    useProviders([SEALED_OPENAI]);
    server.use(
      http.get('*/admin/gateway/platform_models', () =>
        HttpResponse.json({
          items: [],
          total: 0,
          public_project_id: 1,
          model_types: ['llm_model'],
          credential_names: ['platform-openai'],
        }),
      ),
      http.post('*/admin/gateway/providers/4/models', () =>
        HttpResponse.json({ models: ['gpt-4o'], total: 1, truncated: false, type: 'open_ai' }),
      ),
    );
    renderAdminRoute(<LlmProxyProvidersPanel />);

    // The wiring itself is the assertion. A dialog written, exported and never
    // reachable from the row is the dead-wiring failure this repository keeps
    // meeting: every unit test of the dialog passes while nothing opens it.
    await userEvent.click(await screen.findByTestId('admin-provider-adopt-4'));

    // The dialog names the credential it is adopting FROM: a platform model may
    // only link to a published platform provider, and the title is the link.
    expect(await screen.findByText(/Adopt models from platform-openai/)).toBeVisible();
    expect(await screen.findByTestId('adopt-models-list')).toHaveTextContent('gpt-4o');
  });
});
