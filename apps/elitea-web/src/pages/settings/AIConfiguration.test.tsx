/**
 * Regression coverage for issue #80 — the settings page composed two of the
 * four levels the baseline composes.
 *
 * `apps/elitea-ui` renders `pages/settings/AIConfiguration.jsx` ->
 * `Configuration/ModelConfiguration.jsx` -> (project config, configurations
 * panel, MODEL CAPABILITIES, and a copy-the-whole-card button). This app
 * skipped the middle level, so `ModelCapabilitiesSection.tsx` sat in the tree
 * with no importer and the copy button did not exist. This file pins both back
 * onto the page.
 *
 * It also pins the page's OTHER composition-level affordance: "Request a model
 * connection". A component with no mount is the recurring failure on this
 * surface — `ModelCapabilitiesSection` was exactly that — and a dialog's own
 * unit tests cannot see it, because they render the dialog directly.
 */
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRouter,
} from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { resetConfigForTests } from '@/shared/config/get-config';
import { PERMISSIONS } from '@/shared/lib/permissions';

import { server } from '../../test/setup';

import { AIConfiguration } from './AIConfiguration';

const BASE = '/api/v2';
const PROJECT_ID = '1';

const globals = globalThis as unknown as Record<string, unknown>;

/**
 * The catalogue answer of elitea-main: capability BOOLEANS at the top level of
 * the item, and no `capabilities` map
 * (`services/elitea-main/internal/application/configurations/models.go`).
 */
const MODEL_CATALOGUE = {
  total: 1,
  items: [
    {
      name: 'gpt-4o',
      display_name: 'GPT-4o',
      project_id: PROJECT_ID,
      shared: false,
      default: true,
      supports_reasoning: true,
      supports_vision: true,
    },
  ],
  default_model_name: 'gpt-4o',
  default_model_project_id: PROJECT_ID,
};

const LLM_CONFIGURATION = {
  id: 1,
  project_id: PROJECT_ID,
  elitea_title: 'OpenAI GPT-4o',
  label: 'openai',
  type: 'openai',
  section: 'llm',
  shared: false,
  data: { name: 'gpt-4o' },
};

function setConfig(): void {
  globals['elitea_ui_config'] = {
    vite_server_url: 'https://elitea.example.test/api/v2',
    vite_base_uri: '/',
    vite_public_project_id: '99',
  };
  resetConfigForTests();
}

function mockBackend(): void {
  server.use(
    http.get(`${BASE}/configurations/models/${PROJECT_ID}`, () => HttpResponse.json(MODEL_CATALOGUE)),
    http.get(`${BASE}/configurations/configurations/${PROJECT_ID}`, ({ request }) => {
      const section = new URL(request.url).searchParams.get('section');
      return HttpResponse.json({
        total: section === 'llm' ? 1 : 0,
        items: section === 'llm' ? [LLM_CONFIGURATION] : [],
      });
    }),
    http.get(`${BASE}/auth/permissions/prompt_lib/${PROJECT_ID}`, () =>
      HttpResponse.json([{ name: PERMISSIONS.configuration.update, enabled: true }]),
    ),
  );
}

/** `AddModelButton` and `ConfigurationCard` navigate, so a real router is needed. */
function renderPage(): void {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 }, mutations: { retry: false } },
  });
  const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <CssBaseline />
          <AIConfiguration projectId={PROJECT_ID} />
        </ThemeProvider>
      </QueryClientProvider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
  });
  render(<RouterProvider router={router} />);
}

afterEach(() => {
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  resetGeneratedClient();
  Reflect.deleteProperty(globalThis.navigator, 'clipboard');
});

describe('AIConfiguration — the ModelConfiguration layer', () => {
  it('shows the capability chips of the project default model', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockBackend();

    renderPage();

    expect(await screen.findByText('Model Capabilities')).toBeInTheDocument();
    expect(await screen.findByText('Reasoning')).toBeInTheDocument();
    expect(await screen.findByText('Vision')).toBeInTheDocument();

    /* The row follows the configuration accordions, which title themselves at
       level 3. An <h6> here is a two-level jump and axe's `heading-order` rule
       fails the whole screen for it — which is how the E2E a11y check on this
       route went red once a model with capabilities was served. */
    expect(await screen.findByRole('heading', { name: 'Model Capabilities', level: 4 })).toBeInTheDocument();
  });

  it('copies the whole card, capabilities included, as JSON', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockBackend();

    const writeText = vi.fn<(value: string) => Promise<void>>().mockResolvedValue(undefined);
    /* jsdom ships no `navigator.clipboard`, and the property is not writable —
       define it, and take it away again in `afterEach`. */
    Object.defineProperty(globalThis.navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    });

    renderPage();

    const copyButton = await screen.findByRole('button', { name: 'Copy configuration' });
    await userEvent.click(copyButton);

    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    const payload = JSON.parse(writeText.mock.calls[0]?.[0] ?? '{}') as Record<string, unknown>;
    expect(payload['model_capabilities']).toEqual(['reasoning', 'vision']);
    expect(payload['project_configuration']).toEqual({
      server_url: 'https://elitea.example.test/api/v2',
      base_url: 'https://elitea.example.test/llm/v1',
      project_id: PROJECT_ID,
    });
  });

  it('shows no capability row for a model that declares no capability', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockBackend();
    server.use(
      http.get(`${BASE}/configurations/models/${PROJECT_ID}`, () =>
        HttpResponse.json({
          ...MODEL_CATALOGUE,
          items: [{ name: 'gpt-4o', project_id: PROJECT_ID, shared: false, default: true }],
        }),
      ),
    );

    renderPage();

    /* Wait for the panel, so the absence below is a settled answer and not a
       screen that has not rendered yet. */
    expect(await screen.findByText('LLMs')).toBeInTheDocument();
    expect(screen.queryByText('Model Capabilities')).not.toBeInTheDocument();
  });

  it('offers the model-connection request beside the copy and create affordances, for this page project', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockBackend();

    let postedTo = '';
    let postedBody: unknown;
    server.use(
      http.post('*/admin/moderation_status/default/:projectId/:entityId', async ({ params, request }) => {
        postedTo = `${String(params['projectId'])}/${String(params['entityId'])}`;
        postedBody = await request.json();
        return HttpResponse.json({ id: 1, status: 'pending' }, { status: 201 });
      }),
    );

    renderPage();

    /* The three affordances of one decision, on one screen: copy what is
       configured, add a configuration, or — for a member who cannot add one —
       ask an operator for it. */
    const requestButton = await screen.findByRole('button', { name: 'Request a model connection' });
    expect(screen.getByRole('button', { name: 'Copy configuration' })).toBeInTheDocument();
    /* Awaited: the panel's "+" only exists once the configuration list has
       settled, while the two page-level buttons render immediately. */
    expect(await screen.findByRole('button', { name: 'Create configuration' })).toBeInTheDocument();

    await userEvent.click(requestButton);
    await userEvent.type(await screen.findByLabelText('Provider type *'), 'anthropic');
    await userEvent.type(screen.getByLabelText('Description *'), 'We need Claude here.');
    await userEvent.click(screen.getByRole('button', { name: 'Send request' }));

    /* The project the PAGE is rendering, not a separately-resolved one: a
       request filed against the wrong project reaches an operator who cannot
       act on it. */
    await waitFor(() => expect(postedTo).toBe(`${PROJECT_ID}/provider:anthropic`));
    expect(postedBody).toEqual({
      issue_type: 'Model Connection Request',
      description: 'We need Claude here.',
    });
  });

  it('keeps the model-connection request in the page header on both tabs', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockBackend();

    renderPage();

    await screen.findByRole('button', { name: 'Request a model connection' });
    await userEvent.click(screen.getByRole('tab', { name: 'OpenAI Template' }));

    /* It used to be floated over the project-configuration card, so switching
       tabs took it away with the card. It is now part of the page's own
       `DrawerPageHeader` — the title row every other settings tab has and this
       one was missing — which is chrome for the whole page, not for one tab.
       Production keeps its equivalent (the rail's "+ AI Provider") visible the
       same way. */
    expect(screen.getByRole('button', { name: 'Request a model connection' })).toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'OpenAI Template' })).toHaveAttribute('aria-selected', 'true');
  });

  it('shows no capability section on the OpenAI-Template tab', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockBackend();

    renderPage();

    await screen.findByText('Model Capabilities');
    await userEvent.click(screen.getByRole('tab', { name: 'OpenAI Template' }));

    await waitFor(() => expect(screen.queryByText('Model Capabilities')).not.toBeInTheDocument());
  });
});

/**
 * #80, item 4 — the chips describe the SELECTED model, not only the
 * auto-selected default.
 *
 * The chips were mounted against a model nothing could change, so
 * "`getModelCapabilities` works" and "the page shows this model's
 * capabilities" were the same statement. A second model with a DIFFERENT
 * capability set is what makes them two statements.
 */
const TWO_MODEL_CATALOGUE = {
  total: 2,
  items: [
    {
      name: 'gpt-4o',
      display_name: 'GPT-4o',
      project_id: PROJECT_ID,
      shared: false,
      default: true,
      supports_reasoning: true,
      supports_vision: true,
    },
    {
      name: 'text-embedding-3-small',
      display_name: 'Embedding Small',
      project_id: PROJECT_ID,
      shared: false,
      default: false,
      supports_reasoning: false,
      supports_vision: false,
    },
  ],
  default_model_name: 'gpt-4o',
  default_model_project_id: PROJECT_ID,
};

describe('AIConfiguration — the model picker', () => {
  function mockTwoModels(): void {
    server.use(
      http.get(`${BASE}/configurations/models/${PROJECT_ID}`, () => HttpResponse.json(TWO_MODEL_CATALOGUE)),
    );
  }

  it('starts on the project default model', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockBackend();
    mockTwoModels();

    renderPage();

    const picker = await screen.findByLabelText('Model');
    await waitFor(() => expect(picker).toHaveTextContent('GPT-4o'));
    expect(await screen.findByText('Reasoning')).toBeInTheDocument();
  });

  it('changes the chips when a different model is picked', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockBackend();
    mockTwoModels();

    renderPage();

    // The default's chips first, so the change below is a change and not the
    // initial state.
    expect(await screen.findByText('Reasoning')).toBeInTheDocument();
    expect(await screen.findByText('Vision')).toBeInTheDocument();

    await userEvent.click(await screen.findByLabelText('Model'));
    await userEvent.click(await screen.findByRole('option', { name: 'Embedding Small' }));

    // The second model declares neither flag, so the whole section goes.
    await waitFor(() => expect(screen.queryByText('Reasoning')).not.toBeInTheDocument());
    expect(screen.queryByText('Vision')).not.toBeInTheDocument();
    expect(screen.queryByText('Model Capabilities')).not.toBeInTheDocument();
    // …and the picker keeps the model the user chose, so the page is not
    // silently back on the default.
    expect(await screen.findByLabelText('Model')).toHaveTextContent('Embedding Small');
  });

  it('copies the SELECTED model, not the default, once the picker moves', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockBackend();
    mockTwoModels();

    const writeText = vi.fn<(value: string) => Promise<void>>().mockResolvedValue(undefined);
    Object.defineProperty(globalThis.navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
    });

    renderPage();

    await userEvent.click(await screen.findByLabelText('Model'));
    await userEvent.click(await screen.findByRole('option', { name: 'Embedding Small' }));
    await userEvent.click(await screen.findByRole('button', { name: 'Copy configuration' }));

    await waitFor(() => expect(writeText).toHaveBeenCalledTimes(1));
    const payload = JSON.parse(writeText.mock.calls[0]?.[0] ?? '{}') as Record<string, unknown>;
    expect((payload['configuration_options'] as Record<string, unknown>)['model_name']).toBe(
      'text-embedding-3-small',
    );
    expect(payload['model_capabilities']).toEqual([]);
  });
});
