/**
 * create-personal-token.test.tsx — the project binding on
 * `POST /api/v2/auth/token/` (`spec-llm-project-scope` §4, ADR-0018).
 *
 * The page has NO project control. A new token binds to the project the
 * sidebar selects, which the page reads from `useSelectedProjectStore` — the
 * same store `settings/tokens.tsx` reads. The things that can silently go
 * wrong, and are therefore pinned:
 *
 *  1. The request carries the SELECTED project, as a NUMBER (the store holds
 *     a string id; the conversion happens in `bindableProjectId`).
 *  2. With no usable selection the request must be the one that shipped
 *     before the binding existed: NO `project_id` key at all, not
 *     `project_id: null` and not `project_id: 0`. Asserted on the request
 *     body's KEYS, because `toMatchObject` cannot tell an absent key from an
 *     explicit `null` one.
 *  3. The user still sees which project pays, because the binding is
 *     permanent and the API has no update path.
 *  4/5. The 403 `project_forbidden` and 400 `invalid_project_id` failures
 *     reach the user as their own messages. Both use the NESTED envelope
 *     `{"error":{message,type,code}}`, unlike every other failure on this
 *     endpoint, which keeps the FLAT `{"error":"…"}` — one case below feeds a
 *     flat body in to prove the flat shape still degrades to the generic line
 *     instead of throwing or rendering a project message.
 *  6. No project-choosing control comes back. The owner removed the choice,
 *     so a rendered `<select>` here is the defect.
 *
 * Mounts `CreatePersonalTokenPage` through a MINIMAL hand-built router tree
 * for the reason `notifications.test.tsx` documents at length: the real
 * generated tree routes `/settings/*` through `settings-layout.tsx`, whose
 * `SettingsRedirect` navigates away from any leaf that declares no `$tab`.
 */
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import type { AuthContext, RouterContext } from '@/app/router-context';
import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { server } from '../../../test/setup';
import { CreatePersonalTokenPage } from '@/routes/-pages/CreatePersonalToken';

const BASE = '/api/v2';
const PUBLIC_PROJECT_ID = '1';
const TOKEN_PATH = `${BASE}/auth/token/`;

const globals = globalThis as unknown as Record<string, unknown>;

const auth: AuthContext = {
  getUser: () => ({ id: 'u1', personal_project_id: 'p1', permissions: [], publicPermissions: [] }),
  getSelectedProjectId: () => '42',
};

const rootRoute = createRootRoute();
const createTokenRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/settings/create-personal-token',
  component: CreatePersonalTokenPage,
});
const testRouteTree = rootRoute.addChildren([createTokenRoute]);

function mount(): void {
  const history = createMemoryHistory({ initialEntries: ['/settings/create-personal-token'] });
  const router = createRouter({ routeTree: testRouteTree, history, context: { auth } satisfies RouterContext });
  render(
    <AppProviders>
      <RouterProvider router={router} />
    </AppProviders>,
  );
}

/** The sidebar selection this page binds to. */
function selectProject(id: string, name: string): void {
  useSelectedProjectStore.setState({ project: { id, name } });
}

/**
 * Captures every create body. Answers with the BARE record: `eliteaFetch`
 * builds the `{data, status, headers}` envelope itself and `tokenApi`'s
 * `fetchJson` unwraps that one envelope, so a handler that wraps its own
 * `{data: …}` would make `resp.token` undefined — the exact defect
 * `tokenApi.ts`'s own comment records.
 */
function mockCreateOk(): { bodies: Record<string, unknown>[] } {
  const bodies: Record<string, unknown>[] = [];
  server.use(
    http.post(TOKEN_PATH, async ({ request }) => {
      bodies.push((await request.json()) as Record<string, unknown>);
      return HttpResponse.json({
        uuid: 'tok-uuid',
        name: 'my-token',
        token: 'secret-value',
        expires: null,
        project_id: null,
      });
    }),
  );
  return { bodies };
}

function mockCreateFailure(status: number, body: Record<string, unknown>): void {
  server.use(http.post(TOKEN_PATH, () => HttpResponse.json(body, { status })));
}

/** Fills the name and presses Generate, waiting for the form to become valid first. */
async function submit(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  // `findBy…`: the router resolves its match asynchronously, so the form is
  // not in the DOM on the tick after `render`.
  await user.type(await screen.findByLabelText('Name'), 'my-token');
  const generate = screen.getByRole('button', { name: 'Generate' });
  await waitFor(() => expect(generate).toBeEnabled());
  await user.click(generate);
}

beforeEach(() => {
  resetConfigForTests();
  globals['elitea_ui_config'] = {
    vite_server_url: 'https://elitea.example',
    vite_base_uri: '/',
    vite_public_project_id: PUBLIC_PROJECT_ID,
  };
  configureGeneratedClient({ baseUrl: BASE });
  useSelectedProjectStore.setState({ project: null });
});

afterEach(() => {
  resetGeneratedClient();
  resetConfigForTests();
  useSelectedProjectStore.setState({ project: null });
  delete globals['elitea_ui_config'];
});

describe('project binding — the selected project', () => {
  it('sends the selected project as a numeric project_id', async () => {
    selectProject('42', 'Marketing');
    const { bodies } = mockCreateOk();
    mount();

    await submit(userEvent.setup());

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({ name: 'my-token', project_id: 42 });
    expect(typeof bodies[0]!['project_id']).toBe('number');
  });

  it('names the project that pays, and says the binding is permanent', async () => {
    selectProject('42', 'Marketing');
    mount();

    expect(
      await screen.findByText('The project Marketing pays for this token, and you cannot change this later.'),
    ).toBeInTheDocument();
  });
});

describe('project binding — no usable selection', () => {
  it('sends no project_id key at all when no project is selected', async () => {
    const { bodies } = mockCreateOk();
    mount();

    await submit(userEvent.setup());

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(Object.keys(bodies[0]!)).not.toContain('project_id');
    expect(bodies[0]).toMatchObject({ name: 'my-token', expires: { measure: 'days', value: 30 } });
  });

  it('sends no project_id key at all when the selected id is not a positive integer', async () => {
    selectProject('0', 'Not a real project');
    const { bodies } = mockCreateOk();
    mount();

    await submit(userEvent.setup());

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(Object.keys(bodies[0]!)).not.toContain('project_id');
  });

  it('says the token gets no project', async () => {
    mount();

    expect(
      await screen.findByText('This token gets no project, and you cannot change this later.'),
    ).toBeInTheDocument();
  });
});

describe('project binding — no project-choosing control', () => {
  it('renders no project select, and keeps only the expiration one', async () => {
    selectProject('42', 'Marketing');
    mount();

    // The form is mounted before the negative assertions run.
    await screen.findByLabelText('Name');
    const selects = screen.getAllByRole('combobox');
    expect(selects).toHaveLength(1);
    expect(selects[0]).toHaveAccessibleName('Expiration period');
    expect(screen.queryByLabelText('Project')).toBeNull();
    expect(screen.queryByRole('option', { name: 'No project (default)' })).toBeNull();
    expect(screen.queryByRole('option', { name: 'Marketing' })).toBeNull();
  });
});

describe('project binding — the §4 error contract', () => {
  it('renders the membership message for a 403 project_forbidden', async () => {
    selectProject('42', 'Marketing');
    mockCreateFailure(403, {
      error: { message: 'not a member', type: 'permission_error', code: 'project_forbidden' },
    });
    mount();

    await submit(userEvent.setup());

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(
      'You are not a member of this project. Select a different project in the sidebar, then create the token again.',
    );
  });

  it('renders the malformed-project message for a 400 invalid_project_id', async () => {
    selectProject('42', 'Marketing');
    mockCreateFailure(400, {
      error: { message: 'bad project id', type: 'invalid_request_error', code: 'invalid_project_id' },
    });
    mount();

    await submit(userEvent.setup());

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(
      'This project is not a valid choice. Select a different project in the sidebar, then create the token again.',
    );
  });

  /*
   * "Try again" IS WRONG ADVICE FOR A 503. elitea-main answers every
   * /api/v2/auth/token route with 503 `{"error":"token service is not
   * configured"}` while APPLICATION_SECRET_KEY is empty. Retrying can never
   * succeed; only an operator can fix it. A live deployment sat in that state
   * and told the user to try again.
   */
  it('names the operator, not a retry, when the token service is not configured', async () => {
    selectProject('42', 'Marketing');
    mockCreateFailure(503, { error: 'token service is not configured' });
    mount();

    await submit(userEvent.setup());

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(
      'Personal tokens are turned off on this deployment. Ask your administrator to configure the token service.',
    );
    expect(alert).not.toHaveTextContent('Try again.');
  });

  it('falls back to the generic message for the FLAT error envelope every other failure uses', async () => {
    selectProject('42', 'Marketing');
    mockCreateFailure(500, { error: 'internal server error' });
    mount();

    await submit(userEvent.setup());

    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('The system did not create the token. Try again.');
  });
});

describe('expiration unit select — readable options', () => {
  it('colours the select and its option rows from the theme, not the browser popup', async () => {
    mount();
    const select = await screen.findByRole('combobox', { name: 'Expiration period' });
    // Without an explicit colour the options inherit the page's light text in the dark
    // theme and sit on the browser popup's own light background: invisible until hovered.
    expect(getComputedStyle(select).color).toContain('var(--');
    const option = screen.getByRole('option', { name: 'Days' });
    const rule = Array.from(document.querySelectorAll('style'))
      .flatMap((sheet) => Array.from(sheet.sheet?.cssRules ?? []))
      .map((r) => r.cssText)
      .find((text) => text.includes('option') && text.includes('background-color') && select.className.split(' ').some((c) => c !== '' && text.includes(c)));
    expect(option).toBeInTheDocument();
    expect(rule).toBeDefined();
  });
});
