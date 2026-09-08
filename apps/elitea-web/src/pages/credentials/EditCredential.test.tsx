import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { server } from '../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { EditCredential } from './EditCredential';

const BASE = '/api/v2';
const CONTEXT = { projectId: '7', isTeamProject: true, canUpdate: true, canDelete: true };

/**
 * The value a SAVED credential's secret field actually holds once it has been
 * read back. It is a reference, not a key — `secret_sealing.go` replaces the
 * stored api_key with this on every read path. If it ever appears in a request
 * body, the browser has posted a template string to a provider.
 */
const SEALED_SECRET = '{{secret.openai_key}}';

const OPENAI_TYPE_WITH_TEST = {
  type: 'openai',
  section: 'credentials',
  config_schema: {
    title: 'OpenAI',
    properties: { data: { properties: { api_key: { type: 'string', title: 'API Key', secret: true } } } },
  },
  has_test_connection: true,
};

/** Records every request body this test's own handlers see, so the assertions can be about the WIRE and not about a call count on a spy. */
function recordingHandlers(recorded: { url: string; body: string }[]) {
  const record = async ({ request }: { request: Request }) => {
    recorded.push({ url: new URL(request.url).pathname, body: await request.text() });
  };
  return [
    http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE_WITH_TEST])),
    http.get(`${BASE}/configurations/configuration/7/abc`, () =>
      HttpResponse.json({ uid: 'abc', id: 'abc', type: 'openai', elitea_title: 'my-cred', label: 'my-cred', data: { api_key: SEALED_SECRET } }),
    ),
    http.post(`${BASE}/configurations/check_stored_connection/7/abc`, async (info) => {
      await record(info);
      return HttpResponse.json({ success: true, message: 'ok' });
    }),
    http.post(`${BASE}/configurations/check_connection/7/openai`, async (info) => {
      await record(info);
      return HttpResponse.json({});
    }),
  ];
}

function renderEdit() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return renderWithTheme(
    <QueryClientProvider client={client}>
      <EditCredential
        context={CONTEXT}
        credentialUid="abc"
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />
    </QueryClientProvider>,
  );
}

afterEach(() => {
  resetGeneratedClient();
});

describe('EditCredential (ROUTE-025/065 target)', () => {
  it('loads the credential by uid and renders its name', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([{ type: 'openai', config_schema: { title: 'OpenAI', properties: { data: { properties: {} } } } }])));
    server.use(
      http.get(`${BASE}/configurations/configuration/7/abc`, () => HttpResponse.json({ uid: 'abc', type: 'openai', elitea_title: 'my-cred', data: {} })),
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderWithTheme(
      <QueryClientProvider client={client}>
        <EditCredential
          context={CONTEXT}
          credentialUid="abc"
          onSaved={vi.fn()}
          onDiscarded={vi.fn()}
        />
      </QueryClientProvider>,
    );
    await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('my-cred'));
  });

  it('titles the screen "Configuration" in configurationMode (ROUTE-065)', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([{ type: 'openai', config_schema: { title: 'OpenAI', properties: { data: { properties: {} } } } }])));
    server.use(
      http.get(`${BASE}/configurations/configuration/7/abc`, () => HttpResponse.json({ uid: 'abc', type: 'openai', elitea_title: 'my-cred', data: {} })),
    );
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderWithTheme(
      <QueryClientProvider client={client}>
        <EditCredential
          context={CONTEXT}
          credentialUid="abc"
          configurationMode
          onSaved={vi.fn()}
          onDiscarded={vi.fn()}
        />
      </QueryClientProvider>,
    );
    expect(await screen.findByText('Configuration')).toBeInTheDocument();
  });
});

/**
 * THE POINT OF THESE THREE: on a saved credential the browser does not have
 * the secret — it has a `{{secret.NAME}}` reference. So "Test connection" on
 * the edit screen must not be the create screen's call with the loaded `data`
 * as its payload. It names the row instead, and the server redeems the
 * reference itself.
 */
describe('EditCredential — Test connection on a SAVED row', () => {
  it('an untouched secret tests the STORED row, sending no body and no secret', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    const recorded: { url: string; body: string }[] = [];
    server.use(...recordingHandlers(recorded));

    renderEdit();
    await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('my-cred'));
    fireEvent.click(await screen.findByRole('button', { name: 'Test connection' }));

    expect(await screen.findByText('Connection successful')).toBeInTheDocument();

    // Exactly one request, to the stored route, with an EMPTY body.
    expect(recorded).toHaveLength(1);
    expect(recorded[0]?.url).toBe('/api/v2/configurations/check_stored_connection/7/abc');
    expect(recorded[0]?.body).toBe('');
    // And the payload route — the only one that could have carried the
    // credential — was never called at all.
    expect(recorded.some((entry) => entry.url.includes('/check_connection/'))).toBe(false);
    // Belt and braces: nothing resembling the credential left the browser.
    expect(recorded.some((entry) => entry.body.includes('api_key') || entry.body.includes(SEALED_SECRET))).toBe(false);
  });

  it('a re-typed secret goes back to the payload check, carrying the value the user typed', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    const recorded: { url: string; body: string }[] = [];
    server.use(...recordingHandlers(recorded));

    renderEdit();
    await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('my-cred'));
    // A saved row loads in the field's "Secret" mode (the value IS a
    // `{{secret.NAME}}` reference), so re-typing a raw key means switching to
    // the Password tab first — which itself clears the field, and counts as
    // touching the secret.
    fireEvent.click(screen.getByRole('button', { name: 'Password' }));
    fireEvent.change(screen.getByLabelText('API Key'), { target: { value: 'sk-typed-by-hand' } });
    fireEvent.click(await screen.findByRole('button', { name: 'Test connection' }));

    await waitFor(() => expect(recorded).toHaveLength(1));
    expect(recorded[0]?.url).toBe('/api/v2/configurations/check_connection/7/openai');
    // The candidate the user wants tested is the one in the box — and it is
    // the one the browser holds, so sending it is not a leak.
    expect(recorded[0]?.body).toContain('sk-typed-by-hand');
    expect(recorded.some((entry) => entry.url.includes('/check_stored_connection/'))).toBe(false);
  });

  it('renders an unsupported verdict as the "not supported yet" message, not as a failure', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([OPENAI_TYPE_WITH_TEST])),
      http.get(`${BASE}/configurations/configuration/7/abc`, () =>
        HttpResponse.json({ uid: 'abc', id: 'abc', type: 'openai', elitea_title: 'my-cred', label: 'my-cred', data: { api_key: SEALED_SECRET } }),
      ),
      http.post(`${BASE}/configurations/check_stored_connection/7/abc`, () =>
        HttpResponse.json(
          { success: false, unsupported: true, message: 'Checking connection is not supported yet for configuration type openai' },
          { status: 400 },
        ),
      ),
    );

    renderEdit();
    await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('my-cred'));
    fireEvent.click(await screen.findByRole('button', { name: 'Test connection' }));

    expect(await screen.findByText('Checking connection is not supported yet for configuration type openai')).toBeInTheDocument();
    expect(screen.queryByText('Connection test failed')).not.toBeInTheDocument();
  });
});

/**
 * ROUTE-065 is where Settings -> AI Configuration cards now land (gap 6). What
 * this pins is the other half of that fix: the screen a card opens actually
 * LOADS the stored row it was given, in the three shapes a model configuration
 * is made of — the free-text name, the `ai_credentials` REFERENCE to another
 * stored row, and a declared `integer` — and saves it back with PUT.
 *
 * `max_output_tokens` is not an arbitrary field to pick. `classifySchemaField`
 * used to route it to the masked secret widget on the word "tokens", which
 * serialised 16000 as the string "16000" and made the server drop the whole
 * model row. A numeric control here is the evidence that has not come back.
 */
describe('EditCredential loads a stored MODEL configuration (ROUTE-065)', () => {
  const LLM_MODEL_TYPE = {
    type: 'llm_model',
    section: 'models',
    config_schema: {
      title: 'LLM model',
      properties: {
        data: {
          properties: {
            name: { type: 'string', title: 'Model name' },
            /* The registry's own shape for a reference field. */
            ai_credentials: {
              title: 'AI credentials',
              anyOf: [{ type: 'object' }, { type: 'null' }],
              configuration_sections: ['ai_credentials'],
              default: null,
            },
            max_output_tokens: { type: 'integer', title: 'Max output tokens' },
          },
        },
      },
    },
  };

  const STORED_MODEL = {
    id: 42,
    uid: '42',
    type: 'llm_model',
    section: 'models',
    elitea_title: 'qwen3-0.6b',
    label: 'qwen3-0.6b',
    shared: false,
    data: {
      name: 'qwen3-0.6b',
      ai_credentials: { elitea_title: 'local-vllm', private: false },
      max_output_tokens: 16000,
    },
  };

  /** The rows the `ai_credentials` picker draws its options from. */
  const CREDENTIAL_ROWS = {
    items: [{ id: 7, uid: '7', type: 'vllm', section: 'ai_credentials', elitea_title: 'local-vllm', label: 'local-vllm', data: {} }],
    total: 1,
    limit: 200,
    offset: 0,
  };

  function renderStoredModel() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    return renderWithTheme(
      <QueryClientProvider client={client}>
        <EditCredential
          context={CONTEXT}
          credentialUid="42"
          configurationMode
          onSaved={vi.fn()}
          onDiscarded={vi.fn()}
        />
      </QueryClientProvider>,
    );
  }

  it('seeds the form from the stored row and saves it back with PUT', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    const puts: { url: string; body: string }[] = [];
    server.use(
      http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([LLM_MODEL_TYPE])),
      http.get(`${BASE}/configurations/configuration/7/42`, () => HttpResponse.json(STORED_MODEL)),
      http.get(`${BASE}/configurations/configurations/7`, () => HttpResponse.json(CREDENTIAL_ROWS)),
      http.put(`${BASE}/configurations/configuration/7/42`, async ({ request }) => {
        puts.push({ url: new URL(request.url).pathname, body: await request.text() });
        return HttpResponse.json(STORED_MODEL);
      }),
    );

    renderStoredModel();

    await waitFor(() => expect(screen.getByLabelText('Name')).toHaveValue('qwen3-0.6b'));

    // The LINKED credential, shown in the picker rather than as a bare string.
    expect(await screen.findByRole('combobox', { name: 'AI credentials' })).toHaveTextContent('local-vllm');

    // Numeric, not masked: `type="tel"` is the integer branch of
    // `CommonNumberField` (a baseline quirk it preserves), and the stored
    // 16000 is in the box.
    const tokens = screen.getByDisplayValue('16000');
    expect(tokens).toHaveAttribute('type', 'tel');

    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(puts).toHaveLength(1));
    expect(puts[0]?.url).toBe('/api/v2/configurations/configuration/7/42');
    const body = JSON.parse(puts[0]?.body ?? '{}') as Record<string, unknown>;
    // The stable lookup key other domains resolve this row by survives the
    // round trip, and so does every value that was loaded into the form.
    expect(body['elitea_title']).toBe('qwen3-0.6b');
    expect(body['data']).toEqual(STORED_MODEL.data);
  });
});
