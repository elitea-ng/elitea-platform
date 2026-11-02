import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { server } from '../../test/setup';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderCredentialsRoute, withShellCrumbs } from './__tests__/testRouter';

import { CreateCredential } from './CreateCredential';

const BASE = '/api/v2';

const CONTEXT = { projectId: '7', isTeamProject: true, canUpdate: true, canDelete: true };

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

afterEach(() => {
  resetGeneratedClient();
  vi.unstubAllGlobals();
});

function renderPage(ui: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderCredentialsRoute(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

describe('CreateCredential (ROUTE-023/024 target)', () => {
  it('shows the type selector when no credentialType prop is given', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([{ type: 'openai', config_schema: { title: 'OpenAI', properties: {} } }])),
    );
    renderPage(
      <CreateCredential
        context={CONTEXT}
        onCreated={vi.fn()}
        onCancelled={vi.fn()}
      />,
    );
    expect(await screen.findByText('OpenAI')).toBeInTheDocument();
  });

  it('skips the type selector when credentialType is given (ROUTE-024)', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/configurations/available/`, () =>
        HttpResponse.json([{ type: 'openai', config_schema: { title: 'OpenAI', properties: { data: { properties: { api_key: { type: 'string' } } } } } }]),
      ),
    );
    renderPage(
      <CreateCredential
        context={CONTEXT}
        credentialType="openai"
        onCreated={vi.fn()}
        onCancelled={vi.fn()}
      />,
    );
    expect(await screen.findByLabelText('Name')).toBeInTheDocument();
  });

  it('names the screen "New configuration" for the settings-domain reuse (ROUTE-063/064)', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/configurations/available/`, () =>
        HttpResponse.json([{ type: 'openai', config_schema: { title: 'OpenAI', properties: { data: { properties: {} } } } }]),
      ),
    );
    renderPage(
      withShellCrumbs(
        <CreateCredential
          context={CONTEXT}
          credentialType="openai"
          configurationMode
          onCreated={vi.fn()}
          onCancelled={vi.fn()}
        />,
      ),
    );
    await waitFor(() => expect(screen.getByTestId('crumbs')).toHaveTextContent('New configuration'));
  });

  it('threads prefillName through to the Name field', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/configurations/available/`, () =>
        HttpResponse.json([{ type: 'openai', config_schema: { title: 'OpenAI', properties: { data: { properties: {} } } } }]),
      ),
    );
    renderPage(
      <CreateCredential
        context={CONTEXT}
        credentialType="openai"
        prefillName="github_shared_toolkit"
        onCreated={vi.fn()}
        onCancelled={vi.fn()}
      />,
    );
    expect(await screen.findByLabelText('Name')).toHaveValue('github_shared_toolkit');
  });

  it('calls onTypeChosen when a type is picked from the selector', async () => {
    configureGeneratedClient({ baseUrl: BASE });
    server.use(
      http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([{ type: 'openai', config_schema: { title: 'OpenAI', properties: {} } }])),
    );
    const onTypeChosen = vi.fn();
    renderPage(
      <CreateCredential
        context={CONTEXT}
        onCreated={vi.fn()}
        onCancelled={vi.fn()}
        onTypeChosen={onTypeChosen}
      />,
    );
    fireEvent.click(await screen.findByText('OpenAI'));
    expect(onTypeChosen).toHaveBeenCalledWith('openai');
  });
});
