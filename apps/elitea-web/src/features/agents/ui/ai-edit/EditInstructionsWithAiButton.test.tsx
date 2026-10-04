import type { ReactNode } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import type { AgentLlmSettings } from '@/shared/api/agentLlmSettings';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetBackendCapabilitiesForTests, setBackendCapabilityForTests } from '@/shared/config/backendCapabilities';
import { server } from '@/test/setup';

import { EditInstructionsWithAiButton } from './EditInstructionsWithAiButton';

/**
 * §6.2 (R-M1): no `vi.mock()` of application modules — the capability switch
 * has an explicit test setter for exactly this reason, and the two
 * `/configurations/*` reads are served by msw.
 */
const BASE = '/api/v2';
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const llmSettings = { model_name: 'qwen3.5', model_project_id: 1 } as AgentLlmSettings;

function renderButton(node: ReactNode): void {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider
        theme={theme}
        defaultMode={DEFAULT_COLOR_SCHEME}
      >
        {node}
      </ThemeProvider>
    </QueryClientProvider>,
  );
}

/** The model catalogue answers 404: the project default cannot be read. */
const NO_CATALOGUE = Symbol('no catalogue');
type DefaultModel = string | null | typeof NO_CATALOGUE;

/** A deployment that serves the Service Prompt catalogue (ELITEA_CONFIGURATIONS_ENABLED on). */
function serveConfigurations(options: { authored?: string; fallback?: string; defaultModel?: DefaultModel } = {}): void {
  const defaultModel = options.defaultModel ?? null;
  server.use(
    http.get(`${BASE}/configurations/models/7`, () =>
      defaultModel === NO_CATALOGUE
        ? new HttpResponse(null, { status: 404 })
        : HttpResponse.json({ items: [], total: 0, default_model_name: defaultModel }),
    ),
    http.get(`${BASE}/configurations/available/`, () =>
      HttpResponse.json(
        options.fallback === undefined
          ? []
          : [
              {
                type: 'service_prompts',
                config_schema: {
                  properties: { data: { properties: { prompt: { default_by_key: { edit_application_draft: options.fallback } } } } },
                },
              },
            ],
      ),
    ),
    http.get(`${BASE}/configurations/configurations/7`, () =>
      HttpResponse.json({
        items: options.authored === undefined ? [] : [{ type: 'service_prompts', data: { key: 'edit_application_draft', prompt: options.authored } }],
        total: 0,
      }),
    ),
  );
}

/** A default install: `/configurations/*` is not served at all. */
function refuseConfigurations(): void {
  server.use(
    http.get(`${BASE}/configurations/available/`, () => new HttpResponse(null, { status: 404 })),
    http.get(`${BASE}/configurations/configurations/7`, () => new HttpResponse(null, { status: 404 })),
    http.get(`${BASE}/configurations/models/7`, () => new HttpResponse(null, { status: 404 })),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  setBackendCapabilityForTests('llmPredictBlocking', true);
});

afterEach(() => {
  resetGeneratedClient();
  resetBackendCapabilitiesForTests();
});

describe('EditInstructionsWithAiButton — the gate', () => {
  it('renders nothing when the build does not serve the AI routes at all', async () => {
    setBackendCapabilityForTests('llmPredictBlocking', false);
    let called = false;
    server.use(
      http.get(`${BASE}/configurations/available/`, () => {
        called = true;
        return HttpResponse.json([]);
      }),
    );
    renderButton(
      <EditInstructionsWithAiButton
        projectId="7"
        instructions="be helpful"
        llmSettings={llmSettings}
        onApply={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.queryByTestId('ai-edit-instructions-button')).not.toBeInTheDocument());
    // Not merely hidden: a build without the capability must not issue the
    // configuration requests either.
    expect(called).toBe(false);
  });

  it('renders nothing when no Service Prompt resolves — the default install, where /configurations/* is absent', async () => {
    refuseConfigurations();
    renderButton(
      <EditInstructionsWithAiButton
        projectId="7"
        instructions="be helpful"
        llmSettings={llmSettings}
        onApply={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.queryByTestId('ai-edit-instructions-button')).not.toBeInTheDocument());
  });

  it('renders nothing when neither the project nor the version names a model', async () => {
    serveConfigurations({ fallback: 'You rewrite agent instructions.' });
    renderButton(
      <EditInstructionsWithAiButton
        projectId="7"
        instructions="be helpful"
        llmSettings={null}
        onApply={vi.fn()}
      />,
    );
    await waitFor(() => expect(screen.queryByTestId('ai-edit-instructions-button')).not.toBeInTheDocument());
  });

  it('appears once a model and a prompt-type default are both available', async () => {
    serveConfigurations({ fallback: 'You rewrite agent instructions.' });
    renderButton(
      <EditInstructionsWithAiButton
        projectId="7"
        instructions="be helpful"
        llmSettings={llmSettings}
        onApply={vi.fn()}
      />,
    );
    expect(await screen.findByTestId('ai-edit-instructions-button')).toBeInTheDocument();
  });
});

describe('EditInstructionsWithAiButton — generate and apply', () => {
  it('sends the resolved prompt and the current instructions, then applies the reviewed draft', async () => {
    const user = userEvent.setup();
    const onApply = vi.fn();
    serveConfigurations({ authored: 'AUTHORED PROMPT', fallback: 'FALLBACK PROMPT' });
    let body: Record<string, unknown> = {};
    server.use(
      http.post(`${BASE}/elitea_core/predict_llm/prompt_lib/7`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ content: 'Be friendly and concise.' });
      }),
    );

    renderButton(
      <EditInstructionsWithAiButton
        projectId="7"
        instructions="Be helpful."
        llmSettings={llmSettings}
        onApply={onApply}
      />,
    );
    await user.click(await screen.findByTestId('ai-edit-instructions-button'));
    await user.type(screen.getByLabelText('What should change?'), 'friendlier');
    await user.click(screen.getByRole('button', { name: 'Generate draft' }));

    await waitFor(() => expect(screen.getByTestId('text-diff-modified')).toBeInTheDocument());
    // The AUTHORED configuration wins over the type descriptor's default.
    expect(String(body['user_input'])).toContain('AUTHORED PROMPT');
    expect(String(body['user_input'])).toContain('Be helpful.');
    expect(String(body['user_input'])).toContain('friendlier');
    expect(body['llm_settings']).toMatchObject({ model_name: 'qwen3.5' });

    await user.click(screen.getByRole('button', { name: 'Apply' }));
    expect(onApply).toHaveBeenCalledWith('Be friendly and concise.');
  });

  it('reports a generation failure inline and stays on the prompt step', async () => {
    const user = userEvent.setup();
    serveConfigurations({ fallback: 'FALLBACK PROMPT' });
    server.use(
      http.post(`${BASE}/elitea_core/predict_llm/prompt_lib/7`, () => new HttpResponse(null, { status: 500 })),
    );

    renderButton(
      <EditInstructionsWithAiButton
        projectId="7"
        instructions="Be helpful."
        llmSettings={llmSettings}
        onApply={vi.fn()}
      />,
    );
    await user.click(await screen.findByTestId('ai-edit-instructions-button'));
    await user.type(screen.getByLabelText('What should change?'), 'friendlier');
    await user.click(screen.getByRole('button', { name: 'Generate draft' }));

    expect(await screen.findByRole('alert')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Generate draft' })).toBeInTheDocument();
  });

  /** An empty completion is not a draft — applying it would silently blank the field. */
  it('refuses an empty completion rather than proposing it', async () => {
    const user = userEvent.setup();
    serveConfigurations({ fallback: 'FALLBACK PROMPT' });
    server.use(
      http.post(`${BASE}/elitea_core/predict_llm/prompt_lib/7`, () => HttpResponse.json({ content: '   ' })),
    );

    renderButton(
      <EditInstructionsWithAiButton
        projectId="7"
        instructions="Be helpful."
        llmSettings={llmSettings}
        onApply={vi.fn()}
      />,
    );
    await user.click(await screen.findByTestId('ai-edit-instructions-button'));
    await user.type(screen.getByLabelText('What should change?'), 'friendlier');
    await user.click(screen.getByRole('button', { name: 'Generate draft' }));

    expect(await screen.findByRole('alert')).toHaveTextContent('returned nothing to apply');
    expect(screen.queryByTestId('text-diff-modified')).not.toBeInTheDocument();
  });
});

/*
 * Legacy issue 6872: "Edit with AI" ran on the model that was the project
 * default when the agent was CREATED. It must run on the project's CURRENT
 * default, and fall back to the version's model only when there is none.
 */
describe('EditInstructionsWithAiButton — the model', () => {
  async function generateWith(defaultModel: DefaultModel, settings: AgentLlmSettings | null): Promise<Record<string, unknown>> {
    const user = userEvent.setup();
    serveConfigurations({ fallback: 'FALLBACK PROMPT', defaultModel });
    let body: Record<string, unknown> = {};
    server.use(
      http.post(`${BASE}/elitea_core/predict_llm/prompt_lib/7`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ content: 'Draft.' });
      }),
    );
    renderButton(
      <EditInstructionsWithAiButton
        projectId="7"
        instructions="Be helpful."
        llmSettings={settings}
        onApply={vi.fn()}
      />,
    );
    await user.click(await screen.findByTestId('ai-edit-instructions-button'));
    await user.type(screen.getByLabelText('What should change?'), 'shorter');
    await user.click(screen.getByRole('button', { name: 'Generate draft' }));
    await waitFor(() => expect(screen.getByTestId('text-diff-modified')).toBeInTheDocument());
    return body;
  }

  it('uses the project default model, not the model the version was created with', async () => {
    const body = await generateWith('sonnet-4.5', llmSettings);
    expect(body['llm_settings']).toMatchObject({ model_name: 'sonnet-4.5' });
  });

  it('falls back to the version model when the project has no default', async () => {
    const body = await generateWith(null, llmSettings);
    expect(body['llm_settings']).toMatchObject({ model_name: 'qwen3.5' });
  });

  it('falls back to the version model when the catalogue cannot be read', async () => {
    const body = await generateWith(NO_CATALOGUE, llmSettings);
    expect(body['llm_settings']).toMatchObject({ model_name: 'qwen3.5' });
  });

  it('edits a version with no model settings on the project default', async () => {
    const body = await generateWith('sonnet-4.5', null);
    expect(body['llm_settings']).toMatchObject({ model_name: 'sonnet-4.5' });
  });
});
