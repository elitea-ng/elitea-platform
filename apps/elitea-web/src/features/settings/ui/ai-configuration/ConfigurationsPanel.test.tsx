/**
 * ConfigurationsPanel.test.tsx
 *
 * DEFECT this file pins: picking a "Default model" fired the mutation
 * fire-and-forget, and the only report of a failure was
 * `console.error('Error setting default model:', error)`. The select is
 * controlled from query data with no optimistic update, so on failure the
 * value snapped back to the old default and the user saw a silent revert
 * with no reason. The panel now keeps the message per section and renders it
 * beside the select that failed.
 */
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import {
  Outlet,
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from '@tanstack/react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, describe, expect, it } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { PERMISSIONS } from '@/shared/lib/permissions';

import { server } from '../../../../test/setup';

import ConfigurationsPanel from './ConfigurationsPanel';

const BASE = '/api/v2';
const PROJECT_ID = '1';

const globals = globalThis as unknown as Record<string, unknown>;

const LLM_CONFIG = {
  id: 1,
  project_id: PROJECT_ID,
  elitea_title: 'OpenAI GPT-4o',
  label: 'openai',
  type: 'openai',
  section: 'llm',
  shared: false,
  data: { name: 'gpt-4o' },
};

const SECOND_CONFIG = { ...LLM_CONFIG, id: 2, elitea_title: 'Claude Opus', label: 'anthropic', type: 'anthropic', data: { name: 'opus' } };
const THIRD_CONFIG = { ...LLM_CONFIG, id: 3, elitea_title: 'Ollama Local', label: 'ollama', type: 'ollama', data: { name: 'llama' } };

function setConfig(): void {
  globals['elitea_ui_config'] = {
    vite_server_url: 'https://elitea.example',
    vite_base_uri: '/',
    vite_public_project_id: '99',
  };
  resetConfigForTests();
}

/** `ConfigurationCard` navigates through TanStack Router, so a real router must wrap the panel. */
function renderPanel(configs: Record<string, unknown>[] = [LLM_CONFIG]): void {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
          <CssBaseline />
          <ConfigurationsPanel
            configurationsBySection={{ llm: configs }}
            projectId={PROJECT_ID}
            isLoading={false}
          />
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

function mockEditPermission(): void {
  server.use(
    http.get(`${BASE}/auth/permissions/prompt_lib/${PROJECT_ID}`, () =>
      HttpResponse.json([{ name: PERMISSIONS.configuration.update, enabled: true }]),
    ),
  );
}

afterEach(() => {
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  resetGeneratedClient();
});

describe('ConfigurationsPanel default-model saving', () => {
  it('shows the server message beside the select when the save fails', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockEditPermission();
    server.use(
      http.get(`${BASE}/configurations/models/${PROJECT_ID}`, () =>
        HttpResponse.json({ items: [], total: 0, default_model_name: '', default_model_project_id: '' }),
      ),
      http.post(`${BASE}/configurations/models/${PROJECT_ID}`, () =>
        HttpResponse.json({ error: 'insufficient permissions' }, { status: 403 }),
      ),
    );

    renderPanel();

    // The three LLM selects are Default / High-tier / Low-tier, in that order.
    // `DefaultSettingsSelects` passes a React node as the label, so none of
    // them carries an accessible name — index is the only handle available.
    const combobox = (await screen.findAllByRole('combobox'))[0] as HTMLElement;
    await waitFor(() => expect(combobox).not.toHaveAttribute('aria-disabled', 'true'));
    await userEvent.click(combobox);
    await userEvent.click(await screen.findByRole('option', { name: 'OpenAI GPT-4o' }));

    expect(await screen.findByText('insufficient permissions')).toBeInTheDocument();
  });

  it('shows no error message when the save succeeds', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockEditPermission();
    server.use(
      http.get(`${BASE}/configurations/models/${PROJECT_ID}`, () =>
        HttpResponse.json({ items: [], total: 0, default_model_name: '', default_model_project_id: '' }),
      ),
      http.post(`${BASE}/configurations/models/${PROJECT_ID}`, () =>
        HttpResponse.json({ items: [], total: 0 }),
      ),
    );

    renderPanel();

    // The three LLM selects are Default / High-tier / Low-tier, in that order.
    // `DefaultSettingsSelects` passes a React node as the label, so none of
    // them carries an accessible name — index is the only handle available.
    const combobox = (await screen.findAllByRole('combobox'))[0] as HTMLElement;
    await waitFor(() => expect(combobox).not.toHaveAttribute('aria-disabled', 'true'));
    await userEvent.click(combobox);
    await userEvent.click(await screen.findByRole('option', { name: 'OpenAI GPT-4o' }));

    await waitFor(() => expect(screen.queryByText('insufficient permissions')).not.toBeInTheDocument());
  });
});

/**
 * The health dots. The batch route takes ids and NO payload — the stored
 * secret is sealed and this screen never had it — so what these tests pin is
 * the mapping: one response row lands on one card, by id, in each of its
 * three verdicts.
 */
describe('ConfigurationsPanel connection health', () => {
  function mockModels(): void {
    server.use(
      http.get(`${BASE}/configurations/models/${PROJECT_ID}`, () =>
        HttpResponse.json({ items: [], total: 0, default_model_name: '', default_model_project_id: '' }),
      ),
    );
  }

  it('does not check anything until the button is pressed, then paints each verdict on its own card', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockEditPermission();
    mockModels();
    let sentBody = '';
    server.use(
      http.post(`${BASE}/configurations/check_stored_connections/${PROJECT_ID}`, async ({ request }) => {
        sentBody = await request.text();
        return HttpResponse.json([
          { id: '1', success: true },
          { id: '2', success: false, message: 'invalid api key' },
          { id: '3', success: false, unsupported: true },
        ]);
      }),
    );

    renderPanel([LLM_CONFIG, SECOND_CONFIG, THIRD_CONFIG]);

    // Nothing has been checked on mount — this is the whole reason the button
    // exists, and an auto-firing panel would be N provider round trips per
    // page view.
    expect((await screen.findByTestId('configuration-health-dot-1')).getAttribute('data-health')).toBe('unchecked');
    expect(sentBody).toBe('');

    await userEvent.click(screen.getByTestId('check-connections'));

    await waitFor(() => expect(screen.getByTestId('configuration-health-dot-1').getAttribute('data-health')).toBe('ok'));
    expect(screen.getByTestId('configuration-health-dot-2').getAttribute('data-health')).toBe('failed');
    expect(screen.getByTestId('configuration-health-dot-3').getAttribute('data-health')).toBe('unsupported');
    // The failure's own words, not a generic string, are what the dot carries.
    expect(screen.getByTestId('configuration-health-dot-2')).toHaveAttribute('aria-label', 'invalid api key');
    // Ids only: no `data`, no api_key, nothing that could be a secret.
    expect(JSON.parse(sentBody)).toEqual({ configuration_ids: ['1', '2', '3'] });
  });

  it('a batch that fails leaves the dots UNCHECKED and says so once, instead of painting the project red', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockEditPermission();
    mockModels();
    server.use(
      http.post(`${BASE}/configurations/check_stored_connections/${PROJECT_ID}`, () =>
        HttpResponse.json({ error: 'gateway unavailable' }, { status: 503 }),
      ),
    );

    renderPanel([LLM_CONFIG, SECOND_CONFIG]);
    await userEvent.click(await screen.findByTestId('check-connections'));

    // A request that never answered says NOTHING about these rows. Marking
    // them failed — which is what the legacy panel did — reports a healthy
    // project as broken; the reason belongs beside the button that started it.
    expect(await screen.findByText('The connection check could not be run.')).toBeInTheDocument();
    expect(screen.getByTestId('configuration-health-dot-1').getAttribute('data-health')).toBe('unchecked');
    expect(screen.getByTestId('configuration-health-dot-2').getAttribute('data-health')).toBe('unchecked');
  });

  it('re-validating one card repaints it from the status_ok the route returns', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockEditPermission();
    mockModels();
    let revalidateBody = '';
    server.use(
      http.post(`${BASE}/configurations/revalidate/${PROJECT_ID}/1`, async ({ request }) => {
        revalidateBody = await request.text();
        return HttpResponse.json({
          id: 1,
          name: 'gpt-4o',
          type: 'openai',
          section: 'llm',
          status_ok: false,
          status_logs: 'secret openai_key is missing from the vault',
        });
      }),
    );

    renderPanel();

    // The ported card hardcoded "OK" for every row; a re-validate is the one
    // thing that can replace it with the row's real status.
    expect(await screen.findByText(/OK/)).toBeInTheDocument();

    await userEvent.click(screen.getByTestId('configuration-revalidate-1'));

    await waitFor(() => expect(screen.getByTestId('configuration-health-dot-1').getAttribute('data-health')).toBe('failed'));
    expect(screen.getByTestId('configuration-health-dot-1')).toHaveAttribute('aria-label', 'secret openai_key is missing from the vault');
    expect(screen.getByText(/In Progress/)).toBeInTheDocument();
    // Re-deriving admission needs nothing from the client.
    expect(revalidateBody).toBe('');
  });
});

/**
 * DEFECT this pins (gap 6). A card on Settings -> AI Configuration is the ONLY
 * way to open a stored configuration, and every one of them routed to
 * `/settings/create-configuration` — the type chooser for a NEW configuration.
 * The id went into a `state.routeStack[].pagePath` breadcrumb nothing reads, so
 * clicking `local-vllm` opened an empty "New Configuration" form and the stored
 * row could not be edited at all.
 *
 * The assertion is about the DESTINATION, so the router here carries the real
 * edit route (ROUTE-065) rather than a lone root route: a `navigate` to a path
 * this tree does not declare would not be a passing test, it would be a route
 * that never resolves.
 */
describe('ConfigurationsPanel card navigation', () => {
  /** Renders the panel at `/`, with the real edit route mounted beside it. */
  function renderPanelWithRoutes(configs: Record<string, unknown>[] = [LLM_CONFIG]): void {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    });
    const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
    const rootRoute = createRootRoute({
      component: () => (
        <QueryClientProvider client={queryClient}>
          <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
            <CssBaseline />
            <Outlet />
          </ThemeProvider>
        </QueryClientProvider>
      ),
    });
    const indexRoute = createRoute({
      getParentRoute: () => rootRoute,
      path: '/',
      component: () => (
        <ConfigurationsPanel
          configurationsBySection={{ llm: configs }}
          projectId={PROJECT_ID}
          isLoading={false}
        />
      ),
    });
    const editRoute = createRoute({
      getParentRoute: () => rootRoute,
      path: '/settings/edit-configuration/$credential_uid',
      component: function EditConfigurationStub() {
        const { credential_uid: credentialUid } = editRoute.useParams();
        return <div data-testid="edit-configuration-route">{credentialUid}</div>;
      },
    });
    const router = createRouter({
      routeTree: rootRoute.addChildren([indexRoute, editRoute]),
      history: createMemoryHistory({ initialEntries: ['/'] }),
    });
    render(<RouterProvider router={router} />);
  }

  it('opens the clicked configuration in the edit route, carrying its id', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockEditPermission();
    server.use(
      http.get(`${BASE}/configurations/models/${PROJECT_ID}`, () =>
        HttpResponse.json({ items: [], total: 0, default_model_name: '', default_model_project_id: '' }),
      ),
    );

    renderPanelWithRoutes();

    // `getConfigurationDisplayName` reads `label` first, so this is the card's
    // own title row.
    const card = await screen.findByText('openai');
    // The card opens nothing until the permission answer lands: before that it
    // renders "No edit permissions" and the click is a no-op by design.
    await waitFor(() => expect(screen.queryByText('No edit permissions')).not.toBeInTheDocument());
    await userEvent.click(card);

    // The id, not a breadcrumb string: this is what the edit screen loads the
    // stored row by.
    expect(await screen.findByTestId('edit-configuration-route')).toHaveTextContent('1');
  });
});

/**
 * DEFECT this pins (J19b, `credentials.lifecycle.spec.ts:179`). A row saved
 * into a non-LLM section (e.g. "AI Credentials") landed in a COLLAPSED
 * accordion on return from `/settings/create-configuration`, because only
 * the LLMs section ever passed `defaultExpanded: true`. `revealConfigurationId`
 * (the route's `?reveal=` search param) is what fixes it: the section whose
 * `configurations` include that id opens too, everything else keeps its
 * production default.
 */
describe('ConfigurationsPanel reveal (?reveal=) opens the saved row\'s section', () => {
  const AI_CREDENTIAL_CONFIG = {
    id: 42,
    project_id: PROJECT_ID,
    elitea_title: 'GitHub token',
    label: 'GitHub token',
    type: 'custom',
    section: 'ai_credentials',
    shared: false,
    data: {},
  };
  // A second non-LLM, non-matching section — proves `revealConfigurationId`
  // opens ONLY the section holding it, not every non-LLM section at once.
  const EMBEDDING_CONFIG = {
    id: 7,
    project_id: PROJECT_ID,
    elitea_title: 'Embed model',
    label: 'embed',
    type: 'openai',
    section: 'embedding',
    shared: false,
    data: { name: 'text-embed' },
  };

  function mockModels(): void {
    server.use(
      http.get(`${BASE}/configurations/models/${PROJECT_ID}`, () =>
        HttpResponse.json({ items: [], total: 0, default_model_name: '', default_model_project_id: '' }),
      ),
    );
  }

  it('reveal id inside AI Credentials expands that section; LLMs stays expanded too', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockEditPermission();
    mockModels();

    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
    const rootRoute = createRootRoute({
      component: () => (
        <QueryClientProvider client={queryClient}>
          <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
            <CssBaseline />
            <ConfigurationsPanel
              configurationsBySection={{ llm: [LLM_CONFIG], embedding: [EMBEDDING_CONFIG], ai_credentials: [AI_CREDENTIAL_CONFIG] }}
              projectId={PROJECT_ID}
              isLoading={false}
              revealConfigurationId="42"
            />
          </ThemeProvider>
        </QueryClientProvider>
      ),
    });
    const router = createRouter({ routeTree: rootRoute, history: createMemoryHistory({ initialEntries: ['/'] }) });
    render(<RouterProvider router={router} />);

    expect(await screen.findByTestId('ai-providers-section-ai-credentials')).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByTestId('ai-providers-section-llms')).toHaveAttribute('aria-expanded', 'true');
    // A section with no reveal match keeps its own (collapsed) default.
    expect(screen.getByTestId('ai-providers-section-embedding-models')).toHaveAttribute('aria-expanded', 'false');
  });

  it('no reveal id: AI Credentials stays collapsed, LLMs is still expanded', async () => {
    setConfig();
    configureGeneratedClient({ baseUrl: BASE });
    mockEditPermission();
    mockModels();

    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
    const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
    const rootRoute = createRootRoute({
      component: () => (
        <QueryClientProvider client={queryClient}>
          <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
            <CssBaseline />
            <ConfigurationsPanel
              configurationsBySection={{ llm: [LLM_CONFIG], ai_credentials: [AI_CREDENTIAL_CONFIG] }}
              projectId={PROJECT_ID}
              isLoading={false}
            />
          </ThemeProvider>
        </QueryClientProvider>
      ),
    });
    const router = createRouter({ routeTree: rootRoute, history: createMemoryHistory({ initialEntries: ['/'] }) });
    render(<RouterProvider router={router} />);

    expect(await screen.findByTestId('ai-providers-section-llms')).toHaveAttribute('aria-expanded', 'true');
    expect(screen.getByTestId('ai-providers-section-ai-credentials')).toHaveAttribute('aria-expanded', 'false');
  });
});
