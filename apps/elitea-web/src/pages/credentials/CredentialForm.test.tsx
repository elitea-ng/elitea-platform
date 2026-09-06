/**
 * CredentialForm.test.tsx — integration coverage for the create/edit
 * credential screen (unit A7). Real router-free integration: real
 * QueryClient, real MSW-mocked network, no `vi.mock()` of application code
 * (R-M1). Covers ACT-040 (create via Save) and ACT-041 (test connection).
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { server } from '../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { CredentialForm } from './CredentialForm';
import type { CredentialFormContext } from './CredentialForm';

const BASE = '/api/v2';

/**
 * jsdom has no `ResizeObserver` — `CredentialTypeSelector`'s
 * `CategoryItemCard` tiles use `shared/ui/lib/useTextOverflow`, which
 * constructs one. Same stub `CategoryItemCard.test.tsx` already
 * establishes (a no-op is enough; overflow detection runs off mount-time
 * timers, not observer callbacks).
 */
class ResizeObserverStub {
  observe(): void {
    // no-op
  }
  disconnect(): void {
    // no-op
  }
}

beforeEach(() => {
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
});

const CONTEXT: CredentialFormContext = {
  projectId: '7',
  isTeamProject: true,
  canUpdate: true,
  canDelete: true,
};

function renderForm(ui: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return renderWithTheme(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

afterEach(() => {
  resetGeneratedClient();
  vi.unstubAllGlobals();
});

const OPENAI_TYPE = {
  type: 'openai',
  section: 'credentials',
  config_schema: {
    title: 'OpenAI',
    properties: {
      data: {
        properties: {
          api_key: { type: 'string', title: 'API Key', secret: true },
        },
      },
    },
  },
  has_test_connection: true,
};

const OPENAPI_TYPE = {
  type: 'openapi',
  section: 'credentials',
  config_schema: {
    title: 'OpenAPI',
    properties: {
      data: {
        metadata: {
          sections: {
            auth: {
              required: false,
              subsections: [
                { name: 'API Key', fields: ['api_key', 'auth_type', 'custom_header_name'] },
                { name: 'OAuth (Delegated)', fields: ['client_id', 'client_secret', 'oauth_discovery_endpoint', 'scope'] },
                { name: 'OAuth (Client Credentials)', fields: ['client_id', 'client_secret', 'token_url', 'scope', 'method'] },
              ],
            },
          },
        },
        properties: {
          api_key: { anyOf: [{ type: 'string', format: 'password' }, { type: 'null' }], default: null, title: 'Api Key' },
          auth_type: { anyOf: [{ type: 'string', enum: ['Basic', 'Bearer', 'Custom'] }, { type: 'null' }], default: null, title: 'Auth Type' },
          client_id: { anyOf: [{ type: 'string' }, { type: 'null' }], default: null, title: 'Client Id' },
          client_secret: { anyOf: [{ type: 'string', format: 'password' }, { type: 'null' }], default: null, title: 'Client Secret' },
          configuration_uuid: { type: 'string', hidden: true, title: 'Configuration Uuid' },
          custom_header_name: {
            anyOf: [{ type: 'string' }, { type: 'null' }],
            default: null,
            title: 'Custom Header Name',
            visible_when: { field: 'auth_type', value: 'custom' },
          },
          method: { anyOf: [{ type: 'string', enum: ['default', 'Basic'] }, { type: 'null' }], default: null, title: 'Method' },
          oauth_discovery_endpoint: { type: 'string', title: 'Oauth Discovery Endpoint' },
          scope: { type: 'string', title: 'Scope' },
          token_url: { type: 'string', title: 'Token Url' },
        },
      },
    },
  },
};

describe('CredentialForm — create flow', () => {
  /**
   * The chosen type is OWNED BY THE URL, not by this component: picking a
   * tile reports upward through `onTypeChosen`, and the form appears only
   * once the caller feeds that type back in as `mode.credentialType`. The
   * route does that by navigating to `:credentialType` — the baseline's own
   * model (`hooks/credentials/useCredentialSearch.js:29`).
   *
   * This used to be one assertion ("click the tile, the form appears"),
   * which passed against a local `selectedType` that shadowed the prop.
   * Both halves are asserted separately now, because the interesting failure
   * is the component satisfying the first half on its own — that is exactly
   * the state/URL divergence that let Back leave a stale form on screen.
   */
  it('reports the picked type upward and renders the form only when that type is fed back in', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    const onTypeChosen = vi.fn();
    const { rerender } = renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
        onTypeChosen={onTypeChosen}
      />,
    );
    await waitFor(() => expect(screen.getByText('OpenAI')).toBeInTheDocument());
    fireEvent.click(screen.getByText('OpenAI'));

    expect(onTypeChosen).toHaveBeenCalledWith('openai');
    // Still the picker: the component does NOT promote its own selection.
    expect(screen.queryByText('API Key')).not.toBeInTheDocument();

    rerender(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <CredentialForm
          context={CONTEXT}
          mode={{ kind: 'create', credentialType: 'openai' }}
          onSaved={vi.fn()}
          onDiscarded={vi.fn()}
          onTypeChosen={onTypeChosen}
        />
      </QueryClientProvider>,
    );
    expect(await screen.findByText('API Key')).toBeInTheDocument();
  });

  it('falls back to the type selector when the requested type is not a known one', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'no-such-type' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    // The picker, not an empty form — matches the baseline, whose `schema`
    // lookup misses and leaves `initialValues` empty.
    expect(await screen.findByText('OpenAI')).toBeInTheDocument();
    expect(screen.queryByText('API Key')).not.toBeInTheDocument();
  });

  it('skips the type selector when mode.credentialType is already set', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'openai' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    expect(await screen.findByText('API Key')).toBeInTheDocument();
  });

  it('disables Save until a name is entered (ACT-040 precondition)', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'openai' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    await screen.findByText('API Key');
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
  });

  it('ACT-040: Save dispatches POST /configurations/configurations/{projectId} and calls onSaved', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    let capturedBody: unknown;
    server.use(
      http.post(`${BASE}/configurations/configurations/7`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ uid: 'new-1', type: 'openai' });
      }),
    );
    const onSaved = vi.fn();
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'openai' }}
        onSaved={onSaved}
        onDiscarded={vi.fn()}
      />,
    );
    await screen.findByText('API Key');
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'my-openai' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
    expect(capturedBody).toMatchObject({ type: 'openai', elitea_title: 'my-openai', label: 'my-openai' });
  });

  it('surfaces a generic save error without losing the entered name', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    server.use(http.post(`${BASE}/configurations/configurations/7`, () => new HttpResponse(null, { status: 500 })));
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'openai' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    await screen.findByText('API Key');
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'my-openai' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    expect(await screen.findByText('Failed to save credential')).toBeInTheDocument();
    expect(screen.getByLabelText('Name')).toHaveValue('my-openai');
  });

  it('maps a field-specific API error onto the offending schema field, not the generic banner', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    server.use(
      http.post(`${BASE}/configurations/configurations/7`, () => HttpResponse.json({ error: 'api_key is invalid' }, { status: 400 })),
    );
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'openai' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    await screen.findByText('API Key');
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'my-openai' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    expect(await screen.findByText('api_key is invalid')).toBeInTheDocument();
    expect(screen.queryByText('Failed to save credential')).not.toBeInTheDocument();
  });

  it('renders boolean/number/enum schema fields with their own widgets, and submits their edited values', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    const richType = {
      type: 'richtype',
      section: 'credentials',
      config_schema: {
        title: 'Rich Type',
        properties: {
          data: {
            properties: {
              enabled: { type: 'boolean', title: 'Enabled' },
              port: { type: 'number', title: 'Port' },
              region: { type: 'string', title: 'Region', enum: ['us', 'eu'] },
            },
          },
        },
      },
    };
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([richType])));
    let capturedBody: unknown;
    server.use(
      http.post(`${BASE}/configurations/configurations/7`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ uid: 'new-1', type: 'richtype' });
      }),
    );
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'richtype' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    await screen.findByText('Enabled');
    expect(screen.getByText('Port')).toBeInTheDocument();
    expect(screen.getByText('Region')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('checkbox'));
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'my-rich' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(capturedBody).toBeDefined());
    expect((capturedBody as { data: Record<string, unknown> }).data['enabled']).toBe(true);
  });

  it('renders OpenAPI authentication modes without flattening their fields', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAPI_TYPE])));
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'openapi' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );

    expect(await screen.findByRole('radio', { name: 'Anonymous' })).toBeChecked();
    expect(screen.queryByLabelText('Api Key')).not.toBeInTheDocument();
    expect(screen.queryByText('Configuration Uuid')).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole('radio', { name: 'API Key' }));
    expect(screen.getByLabelText('Api Key')).not.toBeRequired();
    expect(screen.queryByLabelText('Client Id')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Custom Header Name')).not.toBeInTheDocument();

    fireEvent.mouseDown(screen.getByLabelText('Auth Type'));
    fireEvent.click(screen.getByRole('option', { name: 'Custom' }));
    expect(screen.getByLabelText('Custom Header Name')).toBeInTheDocument();

    fireEvent.click(screen.getByRole('radio', { name: 'OAuth (Delegated)' }));
    expect(screen.queryByLabelText('Api Key')).not.toBeInTheDocument();
    expect(screen.getByLabelText('Client Id')).not.toBeRequired();
    expect(screen.getByLabelText('Oauth Discovery Endpoint')).toBeInTheDocument();
    expect(screen.queryByLabelText('Token Url')).not.toBeInTheDocument();

    fireEvent.change(screen.getByLabelText('Client Id'), { target: { value: 'client-1' } });
    fireEvent.click(screen.getByRole('radio', { name: 'OAuth (Client Credentials)' }));
    expect(screen.getByLabelText('Client Id')).toHaveValue('client-1');
    expect(screen.getByLabelText('Token Url')).toBeInTheDocument();
    expect(screen.queryByLabelText('Oauth Discovery Endpoint')).not.toBeInTheDocument();
  });

  it('ACT-041: the Test connection button dispatches POST /check_connection/{projectId}/{configType}', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    let url = '';
    server.use(
      http.post(`${BASE}/configurations/check_connection/7/openai`, ({ request }) => {
        url = request.url;
        return HttpResponse.json({});
      }),
    );
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'openai' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    await screen.findByText('API Key');
    fireEvent.click(screen.getByRole('button', { name: 'Test connection' }));

    await waitFor(() => expect(url).toContain('/configurations/check_connection/7/openai'));
    expect(await screen.findByText('Connection successful')).toBeInTheDocument();
  });

  it('reports a failed test connection', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    server.use(http.post(`${BASE}/configurations/check_connection/7/openai`, () => HttpResponse.json({ error: 'bad key' })));
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'openai' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    await screen.findByText('API Key');
    fireEvent.click(screen.getByRole('button', { name: 'Test connection' }));
    expect(await screen.findByText('bad key')).toBeInTheDocument();
  });
});

describe('CredentialForm — edit flow', () => {
  it('loads the existing credential and shows its data, name, and Delete control', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    server.use(
      http.get(`${BASE}/configurations/configuration/7/abc`, () =>
        HttpResponse.json({ uid: 'abc', type: 'openai', elitea_title: 'existing-cred', data: { api_key: 'sk-existing' }, shared: false }),
      ),
    );
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'edit', configId: 'abc' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('existing-cred'));
    expect(screen.getByRole('button', { name: 'Credential actions' })).toBeInTheDocument();
  });

  it('Save dispatches PUT /configurations/configuration/{projectId}/{configId} in edit mode', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    server.use(
      http.get(`${BASE}/configurations/configuration/7/abc`, () =>
        HttpResponse.json({ uid: 'abc', type: 'openai', elitea_title: 'existing-cred', data: { api_key: 'sk-existing' } }),
      ),
    );
    let method = '';
    server.use(
      http.put(`${BASE}/configurations/configuration/7/abc`, ({ request }) => {
        method = request.method;
        return HttpResponse.json({ uid: 'abc' });
      }),
    );
    const onSaved = vi.fn();
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'edit', configId: 'abc' }}
        onSaved={onSaved}
        onDiscarded={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('existing-cred'));
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onSaved).toHaveBeenCalledTimes(1));
    expect(method).toBe('PUT');
  });

  it('deleting through CredentialsControls dispatches DELETE and calls onDiscarded', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    server.use(
      http.get(`${BASE}/configurations/configuration/7/abc`, () =>
        HttpResponse.json({ uid: 'abc', type: 'openai', elitea_title: 'existing-cred', data: {} }),
      ),
    );
    let deleteMethod = '';
    server.use(
      http.delete(`${BASE}/configurations/configuration/7/abc`, ({ request }) => {
        deleteMethod = request.method;
        return HttpResponse.json({});
      }),
    );
    const onDiscarded = vi.fn();
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'edit', configId: 'abc' }}
        onSaved={vi.fn()}
        onDiscarded={onDiscarded}
      />,
    );
    await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('existing-cred'));
    fireEvent.click(screen.getByRole('button', { name: 'Credential actions' }));
    fireEvent.click(screen.getByText('Delete'));
    fireEvent.change(screen.getByRole('textbox'), { target: { value: 'existing-cred' } });
    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));

    await waitFor(() => expect(deleteMethod).toBe('DELETE'));
    expect(onDiscarded).toHaveBeenCalledTimes(1);
  });

  it('seeds Name from the stored label (not elitea_title), and a no-op save never rewrites either field (regression: A7-pages finding 1)', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    server.use(
      http.get(`${BASE}/configurations/configuration/7/abc`, () =>
        HttpResponse.json({
          uid: 'abc',
          type: 'openai',
          elitea_title: 'internal-key-v1',
          label: 'My Prod Key',
          data: { api_key: 'sk-existing' },
        }),
      ),
    );
    let capturedBody: unknown;
    server.use(
      http.put(`${BASE}/configurations/configuration/7/abc`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ uid: 'abc' });
      }),
    );
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'edit', configId: 'abc' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    // The visible Name box shows the freely-editable display label — not
    // the internally-stable elitea_title lookup key — even though the old
    // (buggy) seed order preferred elitea_title.
    await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('My Prod Key'));

    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(capturedBody).toBeDefined());
    // Zero user changes: label must round-trip unchanged, and elitea_title
    // must stay exactly what the server sent — never overwritten with the
    // label value.
    expect(capturedBody).toMatchObject({ elitea_title: 'internal-key-v1', label: 'My Prod Key' });
  });

  it('a deliberate rename updates label but keeps elitea_title stable (regression: A7-pages finding 1)', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    server.use(
      http.get(`${BASE}/configurations/configuration/7/abc`, () =>
        HttpResponse.json({
          uid: 'abc',
          type: 'openai',
          elitea_title: 'internal-key-v1',
          label: 'My Prod Key',
          data: { api_key: 'sk-existing' },
        }),
      ),
    );
    let capturedBody: unknown;
    server.use(
      http.put(`${BASE}/configurations/configuration/7/abc`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({ uid: 'abc' });
      }),
    );
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'edit', configId: 'abc' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('My Prod Key'));
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'My Prod Key v2' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(capturedBody).toBeDefined());
    // The rename reaches `label`; `elitea_title` — what other domains
    // resolve this credential by — is untouched by the rename.
    expect(capturedBody).toMatchObject({ elitea_title: 'internal-key-v1', label: 'My Prod Key v2' });
  });

  it("disables Delete with a reason on a project's last vectorstorage configuration (regression: A7-pages finding 2)", async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    server.use(
      http.get(`${BASE}/configurations/configuration/7/abc`, () =>
        HttpResponse.json({ uid: 'abc', type: 'openai', elitea_title: 'only-pgvector', section: 'vectorstorage', data: {} }),
      ),
    );
    server.use(
      http.get(`${BASE}/configurations/configurations/7`, () =>
        HttpResponse.json({ items: [{ uid: 'abc' }], total: 1, limit: 2, offset: 0, shared: { items: [], total: 0 } }),
      ),
    );
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'edit', configId: 'abc' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('only-pgvector'));
    fireEvent.click(screen.getByRole('button', { name: 'Credential actions' }));
    await waitFor(() => expect(screen.getByRole('menuitem', { name: 'Delete' })).toHaveAttribute('aria-disabled', 'true'));
    expect(screen.getByLabelText('Cannot delete the only pgVector configuration. At least one is required for the project.')).toBeInTheDocument();
  });

  it('keeps Delete enabled when a second configuration exists in the same protected section (regression: A7-pages finding 2)', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    server.use(
      http.get(`${BASE}/configurations/configuration/7/abc`, () =>
        HttpResponse.json({ uid: 'abc', type: 'openai', elitea_title: 'one-of-two', section: 'vectorstorage', data: {} }),
      ),
    );
    server.use(
      http.get(`${BASE}/configurations/configurations/7`, () =>
        HttpResponse.json({
          items: [{ uid: 'abc' }, { uid: 'def' }],
          total: 2,
          limit: 2,
          offset: 0,
          shared: { items: [], total: 0 },
        }),
      ),
    );
    server.use(
      http.delete(`${BASE}/configurations/configuration/7/abc`, () => HttpResponse.json({})),
    );
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'edit', configId: 'abc' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('one-of-two'));
    // The section guard's count query resolves asynchronously (conservative
    // "blocked" default until it settles), so wait for Delete to become
    // enabled before opening the menu. `CredentialsControls` now keeps its
    // Tooltip+Box wrapper tree unconditional across that `canDelete` flip
    // (see its file-level doc comment), so a single open no longer races a
    // remount — this used to need a poll-and-reclick workaround.
    fireEvent.click(screen.getByRole('button', { name: 'Credential actions' }));
    await waitFor(() => {
      expect(screen.getByRole('menuitem', { name: 'Delete' })).not.toHaveAttribute('aria-disabled', 'true');
    });
    fireEvent.click(screen.getByText('Delete'));
    expect(screen.getByText('Delete confirmation')).toBeInTheDocument();
  });

  it('disables Delete when canDelete is false', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    server.use(
      http.get(`${BASE}/configurations/configuration/7/abc`, () =>
        HttpResponse.json({ uid: 'abc', type: 'openai', elitea_title: 'existing-cred', data: {} }),
      ),
    );
    renderForm(
      <CredentialForm
        context={{ ...CONTEXT, canDelete: false }}
        mode={{ kind: 'edit', configId: 'abc' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('existing-cred'));
    fireEvent.click(screen.getByRole('button', { name: 'Credential actions' }));
    expect(screen.getByRole('menuitem', { name: 'Delete' })).toHaveAttribute('aria-disabled', 'true');
  });
});

describe('CredentialForm — configuration mode (ROUTE-063..065)', () => {
  it('titles the screen "Configuration" instead of "Credential"', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE])));
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'openai', configurationMode: true }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    expect(await screen.findByText('Configuration')).toBeInTheDocument();
    expect(screen.queryByText('Credential', { selector: 'h3, h4, h5, h6' })).not.toBeInTheDocument();
  });

  /**
   * The two BLOCKERS this screen shipped, asserted on the payload it sends.
   *
   * The schema below is the `data` half of the registry's real `llm_model`
   * descriptor (`services/elitea-main/internal/application/configurations/
   * current_available_snapshot.json`), which is what `/settings/create-
   * configuration` renders.
   *
   *  1. `max_output_tokens` is a declared `type: 'integer'`. Its key contains
   *     the substring `token`, so the old classifier masked it and the form
   *     posted the STRING `"16000"`. `mapCurrentModelCandidate`
   *     (services/elitea-main/internal/infra/db/repos/models.go) then skipped
   *     the row, and the model was invisible in every model picker.
   *  2. `ai_credentials` is a reference to another stored row. It fell through
   *     to the free-text widget, so the form posted the bare string
   *     `"vllm_creds"`. The gateway's `modelCredentialRef` wants
   *     `{elitea_title, private}`.
   */
  const LLM_MODEL_TYPE = {
    type: 'llm_model',
    section: 'llm',
    config_schema: {
      title: 'LLM model',
      properties: {
        data: {
          properties: {
            // NOT titled "Name": the form's own always-present title box
            // carries that label, and two controls with one accessible name
            // make the query ambiguous. The real schema leaves this untitled.
            name: { type: 'string', title: 'Model Name' },
            ai_credentials: {
              anyOf: [{ $ref: '#/$defs/AiCredentials' }, { type: 'null' }],
              configuration_sections: ['ai_credentials'],
              default: null,
              title: 'Ai Credentials',
            },
            context_window: { type: 'integer', title: 'Context Window', default: 128000 },
            max_output_tokens: { type: 'integer', title: 'Max Output Tokens', default: 16000 },
          },
          required: ['name', 'ai_credentials'],
        },
      },
    },
  };

  function useLlmModelType(credentialTitles: readonly string[]): { readonly body: () => unknown } {
    let captured: unknown;
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([LLM_MODEL_TYPE])));
    server.use(
      http.get(`${BASE}/configurations/configurations/7`, () =>
        HttpResponse.json({
          items: credentialTitles.map((title, index) => ({
            id: index + 1,
            type: 'vllm',
            section: 'ai_credentials',
            elitea_title: title,
            label: title,
          })),
          total: credentialTitles.length,
          limit: 200,
          offset: 0,
        }),
      ),
    );
    server.use(
      http.post(`${BASE}/configurations/configurations/7`, async ({ request }) => {
        captured = await request.json();
        return HttpResponse.json({ uid: 'new-1', type: 'llm_model' });
      }),
    );
    return { body: () => captured };
  }

  it('renders max_output_tokens as a number input, not a masked one, and posts a NUMBER', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    const captured = useLlmModelType(['vllm_creds']);
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'llm_model', configurationMode: true }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );

    const tokensInput = await screen.findByLabelText('Max Output Tokens');
    // The blocker made this an `<input type="password">` seeded empty.
    expect(tokensInput).not.toHaveAttribute('type', 'password');
    expect(tokensInput).toHaveValue('16000');

    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'qwen-local' } });
    fireEvent.change(tokensInput, { target: { value: '32000' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(captured.body()).toBeDefined());
    const data = (captured.body() as { data: Record<string, unknown> }).data;
    expect(data['max_output_tokens']).toBe(32000);
    expect(data['context_window']).toBe(128000);
  });

  it('picks ai_credentials from the project’s stored rows and posts the OBJECT shape', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    const captured = useLlmModelType(['vllm_creds', 'azure_creds']);
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'llm_model', configurationMode: true }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );

    const picker = await screen.findByRole('combobox', { name: 'Ai Credentials' });
    // A picker, not a text box: the blocker rendered a free-text input here.
    expect(picker.tagName).not.toBe('INPUT');
    fireEvent.mouseDown(picker);
    expect(await screen.findByRole('option', { name: 'azure_creds' })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('option', { name: 'vllm_creds' }));

    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'qwen-local' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(captured.body()).toBeDefined());
    const data = (captured.body() as { data: Record<string, unknown> }).data;
    expect(data['ai_credentials']).toEqual({ elitea_title: 'vllm_creds', private: false });
  });

  it('shows the linked credential when an existing model is loaded', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    useLlmModelType(['vllm_creds', 'azure_creds']);
    server.use(
      http.get(`${BASE}/configurations/configuration/7/abc`, () =>
        HttpResponse.json({
          uid: 'abc',
          type: 'llm_model',
          section: 'llm',
          label: 'qwen-local',
          elitea_title: 'qwen-local',
          data: {
            name: 'qwen3',
            ai_credentials: { elitea_title: 'azure_creds', private: false },
            max_output_tokens: 32000,
          },
        }),
      ),
    );
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'edit', configId: 'abc', configurationMode: true }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );

    const picker = await screen.findByRole('combobox', { name: 'Ai Credentials' });
    await waitFor(() => expect(picker).toHaveTextContent('azure_creds'));
  });
});

/**
 * DEFECT: the controller asked for the type catalogue with no `section` at
 * all — `useAvailableConfigurationsType(prefill?.section !== undefined ? ... : {})`
 * fell back to `{}` on every normal entry point, and
 * `buildAvailableConfigurationsTypeUrl` emitted `/configurations/available/?`
 * with an empty query. The server answers an empty section list with EVERY
 * section: 49 descriptors, 136,007 bytes, uncompressed, with no
 * `Cache-Control` or `ETag`.
 *
 * The picker is the visible half. `CredentialForm` passes
 * `availableTypes.data` straight to `<CredentialTypeSelector>`. Create
 * Credential therefore also offered the 17 non-credential types: `llm_model`,
 * `s3`, `environment_settings` and the rest. None of those types carries a
 * `hidden` flag that anything downstream filters on.
 */
describe('CredentialForm — type catalogue scope', () => {
  const LLM_MODEL_TYPE = {
    type: 'llm_model',
    section: 'llm',
    config_schema: { title: 'LLM Model', properties: { data: { properties: {} } } },
  };

  function captureSections(respondWith: readonly unknown[]): { readonly seen: string[][] } {
    const seen: string[][] = [];
    server.use(
      http.get(`${BASE}/configurations/available/`, ({ request }) => {
        seen.push(new URL(request.url).searchParams.getAll('section'));
        return HttpResponse.json(respondWith);
      }),
    );
    return { seen };
  }

  it('asks for the credentials section only, and offers nothing outside it', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    const { seen } = captureSections([OPENAI_TYPE, LLM_MODEL_TYPE]);

    renderForm(
      <CredentialForm context={CONTEXT} mode={{ kind: 'create' }} onSaved={vi.fn()} onDiscarded={vi.fn()} />,
    );

    expect(await screen.findByText('OpenAI')).toBeInTheDocument();
    expect(seen).toEqual([['credentials']]);
  });

  /*
   * The same component serves `/settings/create-configuration`, whose picker
   * needs the model and provider sections. Hardcoding `credentials` would
   * empty that screen — baseline `CreateCredential.jsx:41-51`.
   */
  it('asks for the model and provider sections in configuration mode', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    const { seen } = captureSections([LLM_MODEL_TYPE]);

    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', configurationMode: true }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );

    expect(await screen.findByText('LLM Model')).toBeInTheDocument();
    expect(seen).toEqual([
      ['llm', 'embedding', 'vectorstorage', 'ai_credentials', 'image_generation', 'asr', 'tts'],
    ]);
  });

  it('asks for the section a deep link names', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    const { seen } = captureSections([OPENAI_TYPE]);

    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create' }}
        prefill={{ section: 'service_prompts' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );

    await waitFor(() => expect(seen).toEqual([['service_prompts']]));
  });

  /**
   * REGRESSION guard. On edit the section comes from the loaded detail. A
   * detail body without one gave an empty section list, and an empty list
   * disabled the catalogue query for ever. The descriptor never resolved,
   * so the form showed the Name box and an enabled Save button, and no
   * schema field at all. The edit path must fall back to the unfiltered
   * catalogue instead.
   */
  it('renders the schema fields on edit when the detail body names no section', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    const { seen } = captureSections([OPENAI_TYPE, LLM_MODEL_TYPE]);
    server.use(
      http.get(`${BASE}/configurations/configuration/7/abc`, () =>
        HttpResponse.json({ uid: 'abc', type: 'openai', label: 'No section row', data: { api_key: 'sk-existing' } }),
      ),
    );

    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'edit', configId: 'abc' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );

    expect(await screen.findByText('API Key')).toBeInTheDocument();
    expect(screen.getByDisplayValue('sk-existing')).toBeInTheDocument();
    // One request, and it names no section: the unfiltered catalogue.
    expect(seen).toEqual([[]]);
  });

  /** The normal edit path still asks for the one section the detail names. */
  it('asks for the section the loaded detail names on edit', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    const { seen } = captureSections([OPENAI_TYPE]);
    server.use(
      http.get(`${BASE}/configurations/configuration/7/abc`, () =>
        HttpResponse.json({ uid: 'abc', type: 'openai', section: 'credentials', label: 'Sectioned row', data: {} }),
      ),
    );

    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'edit', configId: 'abc' }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );

    await waitFor(() => expect(seen).toEqual([['credentials']]));
  });
});
