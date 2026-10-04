/**
 * The LLM model form: the Description field with its counter (legacy issue
 * 6766) and the Test connection button (legacy issue 6793). Real QueryClient,
 * msw for the network, no `vi.mock()` of application code (R-M1).
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

class ResizeObserverStub {
  observe(): void {
    // no-op
  }
  disconnect(): void {
    // no-op
  }
}

const CONTEXT: CredentialFormContext = { projectId: '7', isTeamProject: true, canUpdate: true, canDelete: true };

/** The llm_model type as the server publishes it (addLLMModelFormContract). */
const LLM_MODEL_TYPE = {
  type: 'llm_model',
  section: 'llm',
  config_schema: {
    title: 'LLM model',
    properties: {
      data: {
        properties: {
          ai_credentials: {
            anyOf: [{ $ref: '#/$defs/AiCredentials' }, { type: 'null' }],
            configuration_sections: ['ai_credentials'],
            default: null,
          },
          description: {
            anyOf: [{ type: 'string', maxLength: 40 }, { type: 'null' }],
            default: null,
            title: 'Description',
            description: 'A few words on what the model is best for.',
          },
          name: { type: 'string', title: 'Model name' },
        },
        required: ['name', 'ai_credentials'],
      },
    },
  },
  has_test_connection: true,
};

const STORED_MODEL = {
  uid: 'abc',
  type: 'llm_model',
  elitea_title: 'gpt5',
  label: 'GPT-5',
  data: { name: 'gpt-5', ai_credentials: { elitea_title: 'openai_creds', private: false }, description: 'Fast' },
};

function renderForm(ui: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return renderWithTheme(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

function serveType(): void {
  server.use(
    http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([LLM_MODEL_TYPE])),
    http.get(`${BASE}/configurations/configurations/7`, () =>
      HttpResponse.json({ items: [{ elitea_title: 'openai_creds', type: 'open_ai' }], total: 1, shared: { items: [], total: 0 } }),
    ),
  );
}

function renderEdit(): void {
  server.use(http.get(`${BASE}/configurations/configuration/7/abc`, () => HttpResponse.json(STORED_MODEL)));
  renderForm(
    <CredentialForm
      context={CONTEXT}
      mode={{ kind: 'edit', configId: 'abc', configurationMode: true }}
      onSaved={vi.fn()}
      onDiscarded={vi.fn()}
    />,
  );
}

beforeEach(() => {
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
  vi.unstubAllGlobals();
});

describe('LLM model form — Description', () => {
  it('limits the input to 40 characters and counts the characters left', async () => {
    serveType();
    renderEdit();
    const input = await screen.findByLabelText('Description');
    await waitFor(() => expect(input).toHaveValue('Fast'));
    expect(input).toHaveAttribute('maxLength', '40');
    expect(screen.getByTestId('credential-field-counter-description')).toHaveTextContent('36');

    fireEvent.change(input, { target: { value: 'Best for coding and agents' } });
    expect(screen.getByTestId('credential-field-counter-description')).toHaveTextContent('14');
  });
});

describe('LLM model form — Test connection', () => {
  it('tests the UNSAVED form values on the edit screen and reports the time', async () => {
    serveType();
    let body: Record<string, unknown> | undefined;
    let storedCalls = 0;
    server.use(
      http.post(`${BASE}/configurations/check_connection/7/llm_model`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ success: true, message: 'Connected', latency_ms: 900 });
      }),
      http.post(`${BASE}/configurations/check_stored_connection/7/abc`, () => {
        storedCalls += 1;
        return HttpResponse.json({ success: true });
      }),
    );
    renderEdit();
    const modelName = await screen.findByLabelText('Model name');
    await waitFor(() => expect(modelName).toHaveValue('gpt-5'));
    fireEvent.change(modelName, { target: { value: 'gpt-5-mini' } });

    fireEvent.click(screen.getByTestId('credential-test-connection'));
    expect(await screen.findByText(/^Connected in \d+\.\d s$/)).toBeInTheDocument();
    expect(body?.['name']).toBe('gpt-5-mini');
    expect(body?.['ai_credentials']).toEqual({ elitea_title: 'openai_creds', private: false });
    expect(storedCalls).toBe(0);
  });

  it('shows the categorised failure and clears it when the model name changes', async () => {
    serveType();
    server.use(
      http.post(`${BASE}/configurations/check_connection/7/llm_model`, () =>
        HttpResponse.json({ success: false, message: 'Model not found: The API deployment for this resource does not exist.' }, { status: 400 }),
      ),
    );
    renderEdit();
    const modelName = await screen.findByLabelText('Model name');
    await waitFor(() => expect(modelName).toHaveValue('gpt-5'));

    fireEvent.click(screen.getByTestId('credential-test-connection'));
    expect(await screen.findByText('Model not found: The API deployment for this resource does not exist.')).toBeInTheDocument();

    fireEvent.change(modelName, { target: { value: 'gpt-5.1' } });
    await waitFor(() => expect(screen.queryByText(/Model not found/)).not.toBeInTheDocument());
  });

  it('is disabled until AI credentials and a model name are set, and says which is missing', async () => {
    serveType();
    renderForm(
      <CredentialForm
        context={CONTEXT}
        mode={{ kind: 'create', credentialType: 'llm_model', configurationMode: true }}
        onSaved={vi.fn()}
        onDiscarded={vi.fn()}
      />,
    );
    const button = await screen.findByTestId('credential-test-connection');
    expect(button).toBeDisabled();
    fireEvent.mouseOver(button.parentElement as HTMLElement);
    expect(await screen.findByText('Set AI credentials and Model name to test the connection')).toBeInTheDocument();
  });
});
