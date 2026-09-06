/**
 * adminLlmProvidersApi.test.tsx — what a PROVIDER write has to invalidate.
 *
 * MINOR regression (D4). The platform-model listing carries
 * `credential_names`: the platform providers a model may name, and the source
 * of the "Platform provider" select in "Add a platform model"
 * (`PlatformModelsPanel.tsx` passes `data?.credential_names` to
 * `PlatformModelDialog`). Provider mutations invalidated `providerKeys` only,
 * so that listing stayed cached: after adding a provider the select still
 * offered "None" alone until the operator reloaded the page, although the
 * server had already started returning the new name.
 *
 * Asserted through the real cache, not by watching for an
 * `invalidateQueries` call: what matters is that the model listing refetches
 * and the new credential name arrives.
 */
import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { renderHook, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useAdminPlatformModels } from './adminLlmPlatformModelsApi';
import {
  useCreateAdminLlmProvider,
  useDeleteAdminLlmProvider,
  useUpdateAdminLlmProvider,
} from './adminLlmProvidersApi';

const BASE = 'https://elitea.example';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
});

afterEach(() => {
  resetGeneratedClient();
});

/** The listing answers one credential name before the write and two after it. */
function serveGrowingCredentialNames(): void {
  let names = ['platform-openai'];
  server.use(
    http.get('*/admin/gateway/platform_models', () =>
      HttpResponse.json({
        items: [],
        total: 0,
        public_project_id: 1,
        model_types: ['llm_model'],
        credential_names: names,
      }),
    ),
  );
  server.use(
    http.post('*/admin/gateway/providers', () => {
      names = ['platform-openai', 'platform-bedrock'];
      return HttpResponse.json({ id: 2 });
    }),
  );
  server.use(
    http.put('*/admin/gateway/providers/2', () => {
      names = ['platform-openai', 'platform-bedrock'];
      return HttpResponse.json({});
    }),
  );
  server.use(
    http.delete('*/admin/gateway/providers/2', () => {
      names = ['platform-openai'];
      return HttpResponse.json({});
    }),
  );
}

function wrapper(client: QueryClient) {
  return function Wrapper({ children }: { readonly children: ReactNode }): ReactNode {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  };
}

function renderProviderWrites() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return renderHook(
    () => ({
      models: useAdminPlatformModels(),
      create: useCreateAdminLlmProvider(),
      update: useUpdateAdminLlmProvider(),
      remove: useDeleteAdminLlmProvider(),
    }),
    { wrapper: wrapper(client) },
  );
}

describe('admin LLM provider mutations', () => {
  it('refreshes the credential-names listing after a provider is created', async () => {
    serveGrowingCredentialNames();
    const { result } = renderProviderWrites();

    await waitFor(() => expect(result.current.models.data?.credential_names).toEqual(['platform-openai']));

    result.current.create.mutate({
      elitea_title: 'platform-bedrock',
      type: 'amazon_bedrock',
      data: { api_base: 'https://bedrock.example' },
    });

    await waitFor(() =>
      expect(result.current.models.data?.credential_names).toEqual(['platform-openai', 'platform-bedrock']),
    );
  });

  it('refreshes it after a provider is edited', async () => {
    serveGrowingCredentialNames();
    const { result } = renderProviderWrites();

    await waitFor(() => expect(result.current.models.data?.credential_names).toEqual(['platform-openai']));

    result.current.update.mutate({ id: 2, draft: { elitea_title: 'platform-bedrock' } });

    await waitFor(() =>
      expect(result.current.models.data?.credential_names).toEqual(['platform-openai', 'platform-bedrock']),
    );
  });

  it('refreshes it after a provider is deleted, so a withdrawn provider stops being offered', async () => {
    serveGrowingCredentialNames();
    const { result } = renderProviderWrites();

    await waitFor(() => expect(result.current.models.data?.credential_names).toEqual(['platform-openai']));

    result.current.create.mutate({ elitea_title: 'platform-bedrock', type: 'amazon_bedrock', data: {} });
    await waitFor(() =>
      expect(result.current.models.data?.credential_names).toEqual(['platform-openai', 'platform-bedrock']),
    );

    result.current.remove.mutate(2);
    await waitFor(() => expect(result.current.models.data?.credential_names).toEqual(['platform-openai']));
  });
});
