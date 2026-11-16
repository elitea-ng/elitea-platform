import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { resetBackendCapabilitiesForTests, setBackendCapabilityForTests } from '@/shared/config/backendCapabilities';
import { server } from '@/test/setup';

import { SkillAiEditButton } from './SkillAiEditButton';

const BASE = '/api/v2';
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
const value = { name: 'Reviewer', description: 'Reviews code', instructions: 'Be careful.', tags: ['quality'] };

function renderButton(onChange = vi.fn()): ReturnType<typeof vi.fn> {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider
        theme={theme}
        defaultMode={DEFAULT_COLOR_SCHEME}
      >
        <SkillAiEditButton
          projectId="7"
          value={value}
          onChange={onChange}
        />
      </ThemeProvider>
    </QueryClientProvider>,
  );
  return onChange;
}

function serveConfigurations(): void {
  server.use(
    http.get(`${BASE}/configurations/models/7`, () =>
      HttpResponse.json({ items: [], total: 0, default_model_name: 'qwen', default_model_configured: true }),
    ),
    http.get(`${BASE}/configurations/available/`, () =>
      HttpResponse.json([
        {
          type: 'service_prompts',
          config_schema: {
            properties: { data: { properties: { prompt: { default_by_key: { edit_application_draft: 'REWRITE' } } } } },
          },
        },
      ]),
    ),
    http.get(`${BASE}/configurations/configurations/7`, () => HttpResponse.json({ items: [], total: 0 })),
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

// #2104: Skills had no "Edit with AI"; it is the agent editor's own control.
describe('SkillAiEditButton', () => {
  it('offers Edit with AI, reviews without the agent-only fields, and applies the skill fields', async () => {
    const user = userEvent.setup();
    serveConfigurations();
    server.use(
      http.post(`${BASE}/elitea_core/predict_llm/prompt_lib/7`, () =>
        HttpResponse.json({
          content: JSON.stringify({
            name: 'Reviewer',
            description: 'Reviews code carefully.',
            instructions: 'Be very careful.',
            welcome_message: 'Hi!',
          }),
        }),
      ),
    );
    const onChange = renderButton();
    await user.click(await screen.findByRole('button', { name: 'Edit with AI' }));
    await user.type(screen.getByLabelText('What should change?'), 'stricter');
    await user.click(screen.getByRole('button', { name: 'Generate draft' }));
    await waitFor(() => expect(screen.getByLabelText('Instructions')).toBeInTheDocument());
    expect(screen.queryByLabelText('Welcome message')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    expect(onChange).toHaveBeenCalledWith({
      ...value,
      description: 'Reviews code carefully.',
      instructions: 'Be very careful.',
    });
  });

  it('does not apply a proposed name that breaks the skill rules, and sends a skill-specific prompt', async () => {
    const user = userEvent.setup();
    serveConfigurations();
    let sent = '';
    server.use(
      http.post(`${BASE}/elitea_core/predict_llm/prompt_lib/7`, async ({ request }) => {
        sent = JSON.stringify(await request.json());
        return HttpResponse.json({
          content: JSON.stringify({ name: 'x'.repeat(200), description: 'Reviews code carefully.', instructions: 'Be very careful.' }),
        });
      }),
    );
    const onChange = renderButton();
    await user.click(await screen.findByRole('button', { name: 'Edit with AI' }));
    await user.type(screen.getByLabelText('What should change?'), 'stricter');
    await user.click(screen.getByRole('button', { name: 'Generate draft' }));
    await waitFor(() => expect(screen.getByLabelText('Instructions')).toBeInTheDocument());
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    expect(sent).toContain('This is a SKILL');
    expect(sent).not.toContain('Hi!');
    expect(onChange).toHaveBeenCalledWith({ ...value, description: 'Reviews code carefully.', instructions: 'Be very careful.' });
  });

  it('renders nothing when the AI routes are not served', async () => {
    setBackendCapabilityForTests('llmPredictBlocking', false);
    renderButton();
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Edit with AI' })).not.toBeInTheDocument());
  });
});
