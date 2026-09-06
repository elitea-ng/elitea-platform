/**
 * DEFECT (gap G5): the Service Prompts section decided "is this the public
 * project?" with the literal `projectId === '1'` and gated every one of its
 * queries on the answer. On a deployment whose public project is not id 1 the
 * section rendered nothing at all — no request was made, no error was shown,
 * and the admin who could see the prompts on one deployment could not see them
 * on the next.
 *
 * The id is now published by elitea-main on `platform_settings`
 * (`public_project_id`), which is the same value the LLM gateway and the admin
 * provider surface use, so the page and the server cannot disagree.
 *
 * Both directions are asserted. A page that simply dropped the gate would pass
 * the "renders on project 7" case and fail the "hidden on project 7 while the
 * public project is 1" case; a page that kept the literal would do the reverse.
 */
import { render, screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import * as runtimeConfig from '@/shared/config';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { server } from '@/test/setup';

import { ServicePrompts } from './ServicePrompts';

const BASE = '/api/v2';

/** The one prompt row every case below serves, for the project it asks about. */
const PROMPT_ROW = {
  id: 7,
  elitea_title: 'mermaid_quick_fix',
  label: 'Mermaid Quick Fix',
  type: 'service_prompt',
  section: 'service_prompts',
  shared: true,
  data: { key: 'mermaid_quick_fix', prompt: 'fix the diagram' },
};

const AVAILABLE = [
  {
    type: 'service_prompt',
    config_schema: {
      properties: {
        data: {
          properties: {
            key: { enum: ['mermaid_quick_fix'] },
            prompt: { default_by_key: { mermaid_quick_fix: 'the default' } },
          },
        },
      },
    },
  },
];

/** Which project the list endpoint was actually asked for; `null` while unasked. */
let listedProjectId: string | null = null;

function handlers(publicProjectId: number) {
  return [
    http.get(`${BASE}/elitea_core/platform_settings/prompt_lib`, () =>
      HttpResponse.json({ chat_enabled: true, public_project_id: publicProjectId }),
    ),
    http.get(`${BASE}/auth/permissions/prompt_lib/:projectId`, () =>
      HttpResponse.json([{ name: 'configurations.configuration.update', enabled: true }]),
    ),
    http.get(`${BASE}/configurations/available/`, () => HttpResponse.json(AVAILABLE)),
    http.get(`${BASE}/configurations/configurations/:projectId`, ({ params }) => {
      listedProjectId = String(params.projectId);
      return HttpResponse.json({ items: [PROMPT_ROW], total: 1, offset: 0, limit: 100 });
    }),
  ];
}

/**
 * The image's build-time copy. Deliberately '1' in every case below, so a page
 * that still preferred it — or still used the literal — cannot pass the
 * non-default cases by accident.
 */
function stubRuntimeConfig() {
  vi.spyOn(runtimeConfig, 'getConfig').mockReturnValue({
    status: 'ok',
    config: {
      vite_server_url: BASE,
      vite_base_uri: '/',
      vite_public_project_id: '1',
      allow_project_own_llms: false,
    },
  });
}

function mount() {
  render(
    <AppProviders>
      <ServicePrompts />
    </AppProviders>,
  );
}

beforeEach(() => {
  listedProjectId = null;
  configureGeneratedClient({ baseUrl: BASE });
  stubRuntimeConfig();
});

afterEach(() => {
  vi.restoreAllMocks();
  resetGeneratedClient();
  useSelectedProjectStore.setState({ project: null });
});

describe('Service Prompts — the public project is whatever the server says it is', () => {
  it('renders the cards on a deployment whose public project is not id 1', async () => {
    server.use(...handlers(7));
    useSelectedProjectStore.setState({ project: { id: '7', name: 'Public' } });

    mount();

    expect(await screen.findByText('Mermaid Quick Fix')).toBeInTheDocument();
    await waitFor(() => expect(listedProjectId).toBe('7'));
  });

  it('stays hidden on a project that is not the public one', async () => {
    server.use(...handlers(1));
    useSelectedProjectStore.setState({ project: { id: '7', name: 'Private' } });

    mount();

    // The section renders null, so nothing of it is on screen and no
    // configuration read is issued for the project.
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(screen.queryByText('Mermaid Quick Fix')).not.toBeInTheDocument();
    expect(screen.queryByText('Service Prompts')).not.toBeInTheDocument();
    expect(listedProjectId).toBeNull();
  });

  it('behaves exactly as before on a deployment whose public project is id 1', async () => {
    server.use(...handlers(1));
    useSelectedProjectStore.setState({ project: { id: '1', name: 'Public' } });

    mount();

    expect(await screen.findByText('Mermaid Quick Fix')).toBeInTheDocument();
    await waitFor(() => expect(listedProjectId).toBe('1'));
  });

  it('falls back to the image copy when the server does not publish the id', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/platform_settings/prompt_lib`, () =>
        HttpResponse.json({ chat_enabled: true }),
      ),
      ...handlers(1).slice(1),
    );
    useSelectedProjectStore.setState({ project: { id: '1', name: 'Public' } });

    mount();

    expect(await screen.findByText('Mermaid Quick Fix')).toBeInTheDocument();
  });
});
