import type { ReactNode } from 'react';

import { ThemeProvider } from '@mui/material/styles';
import { QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { EliteaApiError, configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { SocketClientContext } from '@/shared/api/socket/client';
import { createTestSocketClient } from '@/shared/api/socket/testing';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import { createTestQueryClient } from '../__tests__/testUtils';
import { resolveToolkitFormDisabled } from './ToolkitEditorParts';
import { ToolkitEditor } from './ToolkitEditor';
import type { ToolkitEditorDeps, ToolkitEditorParticipant, ToolkitEditorProps, ToolkitEditorShellProps } from './ToolkitEditor';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);
const globals = globalThis as unknown as Record<string, unknown>;

function setConfig(): void {
  globals['elitea_ui_config'] = { vite_server_url: 'https://elitea.example', vite_base_uri: '/', vite_public_project_id: 'public-1' };
  resetConfigForTests();
}

/**
 * `ToolkitTypeSelector`'s `CategorySection` rows use `useTextOverflow`
 * (`shared/ui/lib/useTextOverflow.ts`), which constructs a real
 * `ResizeObserver` — jsdom (this project's `node` vitest environment) does
 * not provide one. Same stub `ToolkitTypeSelector.test.tsx` (this same
 * unit) and `pages/credentials/CredentialTypeSelector.test.tsx` already
 * establish for this exact, pre-existing gap.
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
  configureGeneratedClient({ baseUrl: '/api/v2' });
  setConfig();
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
});

afterEach(() => {
  resetGeneratedClient();
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  vi.unstubAllGlobals();
});

/** A minimal, real shell — asserts on `title`/`saveButton`/`children` the same way `BaseEditor` eventually will. */
function TestShell({ title, saveButton, children, onClose }: ToolkitEditorShellProps): ReactNode {
  return (
    <div>
      <h1>{title}</h1>
      <button
        type="button"
        onClick={onClose}
      >
        close
      </button>
      {saveButton}
      {children}
    </div>
  );
}

function renderEditor(toolkit: ToolkitEditorParticipant | null | undefined, depsOverrides: Partial<ToolkitEditorDeps> = {}, propsOverrides: Partial<ToolkitEditorProps> = {}) {
  const createToolkit = depsOverrides.createToolkit ?? vi.fn().mockResolvedValue({ id: 'tk-new', type: 'github', name: 'GitHub' });
  const saveToolkit = depsOverrides.saveToolkit ?? vi.fn().mockResolvedValue({ id: 'tk-1', type: 'github', name: 'GitHub' });

  const queryClient = createTestQueryClient();
  const rootRoute = createRootRoute({
    component: () => (
      <SocketClientContext.Provider value={createTestSocketClient()}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          <ToolkitEditor
            toolkit={toolkit}
            isVisible
            deps={{ renderShell: (props) => <TestShell {...props} />, createToolkit, saveToolkit, ...depsOverrides }}
            {...propsOverrides}
          />
        </ThemeProvider>
      </SocketClientContext.Provider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => 'proj-1' } },
  });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return { createToolkit, saveToolkit };
}

describe('ToolkitEditor', () => {
  it('renders nothing when toolkit is null', () => {
    const { container } = render(<div />);
    renderEditor(null);
    expect(container).toBeDefined();
    expect(screen.queryByRole('heading')).not.toBeInTheDocument();
  });

  it('create mode: shows the type selector first, then the toolkit form once a type is picked', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })));
    const user = userEvent.setup();
    renderEditor({ isCreating: true, isMCP: false });

    expect(await screen.findByText('New Toolkit')).toBeInTheDocument();
    expect(screen.getByText('Choose the toolkit type')).toBeInTheDocument();

    await user.click(await screen.findByText('GitHub'));

    await waitFor(() => expect(screen.queryByText('Choose the toolkit type')).not.toBeInTheDocument());
  });

  it('edit mode: fetches the real toolkit detail by id and shows its name as the title', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tool/prompt_lib/:projectId/:toolkitId', () =>
        HttpResponse.json({ id: 'tk-1', type: 'github', name: 'My GitHub', description: '', settings: {}, meta: {}, created_at: '2026-01-01T00:00:00Z', author_id: 1 }),
      ),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' } } })),
    );
    renderEditor({ id: 'tk-1' });

    expect(await screen.findByText('My GitHub')).toBeInTheDocument();
  });

  it('edit mode: calls onCloseToolkitEditor and deps.onEditorClosed when the shell closes', async () => {
    server.use(
      http.get('/api/v2/elitea_core/tool/prompt_lib/:projectId/:toolkitId', () =>
        HttpResponse.json({ id: 'tk-1', type: 'github', name: '', description: '', settings: {}, meta: {}, created_at: '2026-01-01T00:00:00Z', author_id: 1 }),
      ),
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})),
    );
    const onCloseToolkitEditor = vi.fn();
    const onEditorClosed = vi.fn();
    const user = userEvent.setup();
    renderEditor({ id: 'tk-1' }, { onEditorClosed }, { onCloseToolkitEditor });

    await user.click(await screen.findByRole('button', { name: 'close' }));

    expect(onCloseToolkitEditor).toHaveBeenCalledTimes(1);
    expect(onEditorClosed).toHaveBeenCalledTimes(1);
  });

  it('create mode: New MCP title when isMCP is true', async () => {
    server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})));
    renderEditor({ isCreating: true, isMCP: true });

    expect(await screen.findByText('New MCP')).toBeInTheDocument();
  });
});

/**
 * #613 — the server's per-field save refusal reaching the form.
 *
 * `CreateToolkitButton`/`SaveToolkitButton` each catch their own rejection into
 * an `onError` that nothing in this app ever supplied, and `ToolkitForm`'s
 * `toolkitValidation` channel had no production producer at all, so a refused
 * save vanished twice over. This exercises both halves through the real editor:
 * the `deps` wrapper that records the rejection, and the prop that hands it to
 * the form.
 *
 * The OBSERVABLE is the second click. A recorded `settings_errors` entry keyed
 * to a field becomes a `serverToolErrors` entry, which merges into `hasErrors`,
 * which is what `CreateToolkitButton` gates on — so a body the server has
 * already refused is not sent again. Nothing about that works unless every hop
 * is wired, and it needs no assumption about which field component the form
 * happens to render for a given schema.
 *
 * VERIFIED to discriminate: removing `toolkitValidation={toolkitValidation}`
 * from `ToolkitEditor.tsx`'s `<ToolkitEditorBody>` fails this test and nothing
 * else in this file; so does dropping the `depsWithSaveErrors` wrapper.
 */
describe('a refused save is not re-issued', () => {
  it('records the server settings_errors and blocks the second Save', async () => {
    // `name_required: false` so the form starts with NO local error: this test
    // is about the SERVER's errors reaching `hasErrors`, and a blank required
    // name would block the first Save before any request went out.
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' }, name_required: false } })),
    );
    const user = userEvent.setup();
    // The real transport shape: a rejected `eliteaFetch` throws
    // `EliteaApiError` carrying `failure.body`, NOT the `.data` the injected
    // prop type declares. A supplier written to the declared type reads
    // `undefined` here and this test fails.
    const createToolkit = vi.fn().mockRejectedValue(
      new EliteaApiError({
        kind: 'http',
        status: 400,
        url: '/api/v2/elitea_core/tools/prompt_lib/proj-1',
        body: {
          valid: false,
          settings_errors: [
            {
              loc: ['settings', 'github_configuration'],
              msg: 'Your configuration does not match any available configurations.',
              type: 'value_error',
              code: 'configuration_not_found',
            },
          ],
        },
      }),
    );
    renderEditor({ isCreating: true, isMCP: false }, { createToolkit });

    await user.click(await screen.findByText('GitHub'));
    const save = await screen.findByRole('button', { name: /^Create$/i });
    await waitFor(() => expect(save).not.toBeDisabled());

    await user.click(save);
    await waitFor(() => expect(createToolkit).toHaveBeenCalledTimes(1));

    await user.click(save);
    await waitFor(() => expect(createToolkit).toHaveBeenCalledTimes(1));
  }, 30_000);
});

/** #2016 — a refusal the server did not key to a field used to be silent in the chat editor (no banner exists there). */
describe('a save the server refused without naming a field', () => {
  it('is said in a snackbar, in the server own words', async () => {
    server.use(
      http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({ github: { metadata: { label: 'GitHub' }, name_required: false } })),
    );
    const user = userEvent.setup();
    const createToolkit = vi.fn().mockRejectedValue(
      new EliteaApiError({ kind: 'http', status: 502, url: '/api/v2/elitea_core/tools/prompt_lib/proj-1', body: { error: 'The toolkit service is not reachable.' } }),
    );
    renderEditor({ isCreating: true, isMCP: false }, { createToolkit });

    await user.click(await screen.findByText('GitHub'));
    const save = await screen.findByRole('button', { name: /^Create$/i });
    await waitFor(() => expect(save).not.toBeDisabled());
    await user.click(save);

    expect(await screen.findByText('The toolkit service is not reachable.')).toBeInTheDocument();
  }, 30_000);
});

/**
 * [R3 regression] Baseline: `ToolkitEditor.jsx:303`'s `disabled={isPublic &&
 * !hasPublicProjectAccess}` — editing a public-project toolkit from chat is
 * locked only for users who LACK explicit public-project access; a user who
 * HAS it can still edit. Before this fix, `ToolkitEditorParts.tsx`/
 * `ToolkitEditor.tsx` had no `hasPublicProjectAccess` counterpart anywhere
 * (`disabled: isPublic` was passed straight through, unconditionally, with
 * no override mechanism reachable at all) — a public-project toolkit was
 * locked for EVERY user, including ones who previously had explicit
 * public-project write access. `resolveToolkitFormDisabled` is the exact
 * function `ToolkitEditor.tsx` calls at its one real call site
 * (`disabled={resolveToolkitFormDisabled(isPublic, deps.hasPublicProjectAccess)}`),
 * so this directly regression-tests the restored formula (confirmed by
 * reverting the fix locally and re-running: the import itself doesn't
 * exist pre-fix, so this whole suite fails to even compile/import against
 * the pre-fix file).
 */
describe('resolveToolkitFormDisabled', () => {
  it('is false for a non-public toolkit, regardless of hasPublicProjectAccess', () => {
    expect(resolveToolkitFormDisabled(false, false)).toBe(false);
    expect(resolveToolkitFormDisabled(false, true)).toBe(false);
    expect(resolveToolkitFormDisabled(false, undefined)).toBe(false);
  });

  it('is true for a public toolkit when the user has no public-project access (baseline default, conservative)', () => {
    expect(resolveToolkitFormDisabled(true, false)).toBe(true);
    expect(resolveToolkitFormDisabled(true, undefined)).toBe(true);
  });

  it('is false for a public toolkit when the user HAS explicit public-project access — the restored baseline override', () => {
    expect(resolveToolkitFormDisabled(true, true)).toBe(false);
  });
});
