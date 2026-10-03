/**
 * Regression coverage for a genuine non-terminating render loop found in
 * `SecretsContent` (`Secrets.tsx`).
 *
 * ROOT CAUSE (verified directly, see below): `useListSecretsQuery` returns
 * `UseQueryResult<Secret[], Error>` — `data` is `Secret[] | undefined`. The
 * component used to destructure it as `const { data: secrets = [] } = …`.
 * A destructuring default allocates a BRAND NEW `[]` literal on every
 * render in which `data` is `undefined` — exactly the state the query sits
 * in for as long as it's disabled (no project selected, this file's
 * `projectId` defaults to `''`) or never resolves. That fresh array
 * reference then fed a `useEffect([secrets, isFetching, setRows])`: React
 * saw a "changed" dependency on every render, re-ran the effect, and
 * called `setRows` with a new array reference every time — even though the
 * actual contents ([]) never changed — which triggered a re-render, which
 * allocated a fresh `[]` again, forever.
 *
 * CONFIRMED PRE-FIX: mounting `SecretsContent` with the default (no
 * project selected) `useSelectedProjectStore` state hung the vitest
 * process — `timeout 40 npx vitest run …` had to SIGTERM it (exit 124),
 * matching the reported "CPU pegs at 100%, has to be killed after 30+
 * seconds" symptom exactly. A `Profiler`-based render counter on the same
 * pre-fix mount captured unbounded, linear growth with no sign of
 * levelling off: 500 renders in ~410ms, 5000 renders in ~2.4s (roughly
 * 2000 renders/sec) before the counter's own circuit breaker cut it off.
 *
 * The first `it` below is that same reproduction, kept as a permanent
 * regression test: a `Profiler.onRender` counter with a low, generous
 * circuit-breaker threshold (`RENDER_LIMIT`) that (a) bounds worst-case
 * test runtime even if the bug regresses, since a genuinely runaway loop
 * trips the breaker within tens of milliseconds at the observed rate, and
 * (b) fails with a clear message either way the breaker's throw manifests
 * (some React builds swallow an error thrown from `onRender` rather than
 * propagating it synchronously through `render()` — the `caughtError`
 * re-throw handles the propagating case, the trailing `renderCount`
 * assertion handles the swallowed case).
 */
import { Profiler } from 'react';

import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { server } from '@/test/setup';

import { SECRETS_SKELETON_TESTID } from '@/features/settings/ui/secrets/SecretsTable';
import { SECRETS_LOAD_ERROR_TESTID } from '@/features/settings/ui/secrets/SecretsLoadError';

import { SecretsContent } from './Secrets';

const BASE = '/api/v2';
const PERMISSIONS_PATH = `${BASE}/auth/permissions/prompt_lib/:projectId`;
const SECRETS_PATH = `${BASE}/secrets/secrets/default/:projectId`;

/** A generous ceiling no legitimately-settling mount should ever approach. */
const RENDER_LIMIT = 100;

function noop(): void {
  // intentionally empty — satisfies SecretsContentProps.onSearchChange
}

function mountSecretsContent(onRender: () => void): ReturnType<typeof render> {
  return render(
    <AppProviders>
      <Profiler id="secrets-render-guard" onRender={onRender}>
        <SecretsContent shouldCreate={false} search="" onSearchChange={noop} />
      </Profiler>
    </AppProviders>,
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  useSelectedProjectStore.setState({ project: null });
});

afterEach(() => {
  resetGeneratedClient();
  useSelectedProjectStore.setState({ project: null });
});

describe('SecretsContent — render-loop regression', () => {
  it('does not enter a runaway render loop when no project is selected (query stays disabled forever)', () => {
    let renderCount = 0;
    let caughtError: Error | undefined;

    try {
      mountSecretsContent(() => {
        renderCount += 1;
        if (renderCount > RENDER_LIMIT) {
          throw new Error(
            `Runaway render loop detected: SecretsContent re-rendered ${renderCount} times ` +
              `(limit ${RENDER_LIMIT}) with no project selected — the destructuring-default-into-` +
              'a-dependency-array bug is back.',
          );
        }
      });
    } catch (err) {
      caughtError = err instanceof Error ? err : new Error(String(err));
    }

    if (caughtError) throw caughtError;
    expect(renderCount).toBeLessThan(RENDER_LIMIT);
  });
});

describe('SecretsContent — happy path', () => {
  it('renders the fetched secrets as rows for a real project id', async () => {
    useSelectedProjectStore.setState({ project: { id: 'proj-1', name: 'Acme' } });
    server.use(
      http.get(PERMISSIONS_PATH, () =>
        HttpResponse.json([
          { name: 'configuration.secrets.secret.list', enabled: true },
          { name: 'configuration.secrets.secret.unsecret', enabled: true },
        ]),
      ),
      http.get(SECRETS_PATH, () =>
        HttpResponse.json([
          { name: 'API_KEY', secret_name: 'API_KEY', is_default: false },
          { name: 'DB_PASSWORD', secret_name: 'DB_PASSWORD', is_default: true },
        ]),
      ),
    );

    render(
      <AppProviders>
        <SecretsContent shouldCreate={false} search="" onSearchChange={noop} />
      </AppProviders>,
    );

    await waitFor(() => {
      expect(screen.getAllByText('API_KEY').length).toBeGreaterThan(0);
    });
    expect(screen.getAllByText('DB_PASSWORD').length).toBeGreaterThan(0);
  });
});

/**
 * #137 defects 2 + 3, through the real page wiring rather than the individual
 * components: a project with NO secrets must render a usable table (not eight
 * skeletons forever), and a brand-new row must be fillable end to end so
 * `createSecret` actually fires. Before the fix the empty list short-circuited
 * to skeletons, and the new row's value cell was read-only — `onSave`'s
 * `if (row.name && row.secretValue)` guard could never hold, so no POST was
 * ever issued and the row vanished from state.
 *
 * The POST URL asserted here is also the client-side half of defect 1: it is
 * `/api/v2/secrets/secrets/default/{projectId}` — the pylon shape
 * (plugin `secrets` + resource module `secrets.py` + mode `default`) that
 * elitea-main serves again since #151, pinned on the Go side by
 * TestRouterServesSecretsUnderThePluginPrefix. It was briefly
 * `/api/v2/secrets/prompt_lib/...` — a path and a mode this domain never
 * had — which #137 moved the server onto, breaking elitea-sdk, admin_ui and
 * qa/elitea-api-testing.
 */
describe('SecretsContent — empty list and secret creation (issue 137)', () => {
  // `.create` is granted here because these two tests CREATE. Since #402 the
  // page renders the add button, and honours `?createSecret=1`, only for a
  // caller that holds `configuration.secrets.secret.create` — a viewer now
  // holds the list and none of the writes, so an ungated create row would open
  // for a caller whose POST can only answer 403.
  function permissionsHandler(): ReturnType<typeof http.get> {
    return http.get(PERMISSIONS_PATH, () =>
      HttpResponse.json([
        { name: 'configuration.secrets.secret.list', enabled: true },
        { name: 'configuration.secrets.secret.unsecret', enabled: true },
        { name: 'configuration.secrets.secret.create', enabled: true },
      ]),
    );
  }

  it('renders a table with an empty state, not a permanent skeleton, when the project has no secrets', async () => {
    useSelectedProjectStore.setState({ project: { id: 'proj-1', name: 'Acme' } });
    let listCalls = 0;
    server.use(
      permissionsHandler(),
      http.get(SECRETS_PATH, () => {
        listCalls += 1;
        return HttpResponse.json([]);
      }),
    );

    render(
      <AppProviders>
        <SecretsContent shouldCreate={false} search="" onSearchChange={noop} />
      </AppProviders>,
    );

    // Wait for the list request itself, so this asserts the SETTLED state and
    // not the pre-fetch render that happens before the query is enabled.
    await waitFor(() => {
      expect(listCalls).toBeGreaterThan(0);
    });
    await waitFor(() => {
      expect(screen.queryAllByTestId(SECRETS_SKELETON_TESTID)).toHaveLength(0);
      expect(screen.getByRole('grid')).toBeInTheDocument();
    });
    expect(screen.getByText('No secrets')).toBeInTheDocument();
    expect(screen.getByText('Rows per page')).toBeInTheDocument();
  }, 20_000);

  it('creates a secret typed into a new row', async () => {
    useSelectedProjectStore.setState({ project: { id: 'proj-1', name: 'Acme' } });
    let listCalls = 0;
    const created: { url: string; body: unknown }[] = [];
    server.use(
      permissionsHandler(),
      http.get(SECRETS_PATH, () => {
        listCalls += 1;
        return HttpResponse.json([]);
      }),
      http.post(SECRETS_PATH, async ({ request }) => {
        created.push({ url: request.url, body: await request.json() });
        return HttpResponse.json({});
      }),
    );

    render(
      <AppProviders>
        <SecretsContent shouldCreate search="" onSearchChange={noop} />
      </AppProviders>,
    );

    await waitFor(() => {
      expect(listCalls).toBeGreaterThan(0);
    });
    // The new row must expose BOTH a name and a value input. Before the fix
    // the value column rendered the read-only display cell, so there was
    // exactly one.
    await waitFor(() => {
      expect(screen.queryAllByTestId(SECRETS_SKELETON_TESTID)).toHaveLength(0);
      expect(within(screen.getByRole('grid')).getAllByRole('textbox')).toHaveLength(2);
      expect(screen.getByRole('button', { name: 'Save' })).toBeInTheDocument();
    });

    const [nameInput, valueInput] = within(screen.getByRole('grid')).getAllByRole('textbox');
    fireEvent.change(nameInput!, { target: { value: 'API_KEY' } });
    fireEvent.change(valueInput!, { target: { value: 's3cret' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(created).toHaveLength(1);
    });
    expect(created[0]!.url).toContain('/api/v2/secrets/secrets/default/proj-1');
    expect(created[0]!.body).toEqual({ name: 'API_KEY', value: 's3cret' });
  }, 20_000);
});

/**
 * #402 at the page: a viewer reads the secret NAMES and changes nothing.
 *
 * The legacy matrix withheld `configuration.secrets.secret.list` from the
 * viewer, so a viewer saw a permanently empty table. Migration 0083 grants the
 * list. That makes the viewer the first role that can read this page and hold
 * none of the five write strings, and the page's controls were not gated for
 * that role: the add button, the row menu and the copy button all rendered and
 * could only produce a 403. Two of the three failed with no toast at all.
 *
 * Both directions are measured. The entitled caller is covered by the create
 * test above, which still presses the add flow end to end.
 */
describe('SecretsContent — a viewer lists names and writes nothing (issue 402)', () => {
  function viewerPermissionsHandler(): ReturnType<typeof http.get> {
    return http.get(PERMISSIONS_PATH, () =>
      HttpResponse.json([{ name: 'configuration.secrets.secret.list', enabled: true }]),
    );
  }

  it('shows the names, hides every write control, and ignores ?createSecret=1', async () => {
    useSelectedProjectStore.setState({ project: { id: 'proj-1', name: 'Acme' } });
    const writes: string[] = [];
    server.use(
      viewerPermissionsHandler(),
      http.get(SECRETS_PATH, () =>
        HttpResponse.json([{ name: 'API_KEY', secret_name: 'API_KEY', is_default: false }]),
      ),
      http.post(SECRETS_PATH, ({ request }) => {
        writes.push(request.url);
        return HttpResponse.json({});
      }),
    );

    render(
      <AppProviders>
        {/* `shouldCreate` is the `?createSecret=1` flag, which a bookmark or the
            global create menu can set for any caller. */}
        <SecretsContent shouldCreate search="" onSearchChange={noop} />
      </AppProviders>,
    );

    // The grant works: the name the toolkit configuration references is on screen.
    await waitFor(() => {
      expect(screen.getAllByText('API_KEY').length).toBeGreaterThan(0);
    });

    // No control that could only answer 403.
    expect(screen.queryByRole('button', { name: 'Create new secret' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'More actions' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Copy' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Show' })).not.toBeInTheDocument();

    // The URL flag opens no editable row, so nothing can be typed and posted.
    expect(within(screen.getByRole('grid')).queryAllByRole('textbox')).toHaveLength(0);
    expect(writes).toHaveLength(0);
  }, 20_000);
});

/**
 * UI-UX-1(b): a FAILED list is not an empty project.
 *
 * The page handed the table `[]` on a list error, and the table showed "No
 * secrets". F1 made that the live symptom: a project with a default model
 * selected held an integer in its vault, the list answered 500, and the page
 * said the project had no secrets — inviting the create the backend's
 * unreadable-vault guard refuses. The page now shows an error state, and a
 * retry that succeeds shows the rows.
 */
describe('SecretsContent — a failed list shows an error state (UI-UX-1(b))', () => {
  it('renders the error state instead of the "No secrets" empty state, and recovers on retry', async () => {
    useSelectedProjectStore.setState({ project: { id: 'proj-1', name: 'Acme' } });
    let failing = true;
    server.use(
      http.get(PERMISSIONS_PATH, () =>
        HttpResponse.json([
          { name: 'configuration.secrets.secret.list', enabled: true },
          { name: 'configuration.secrets.secret.unsecret', enabled: true },
        ]),
      ),
      http.get(SECRETS_PATH, () =>
        failing
          ? HttpResponse.json({ error: 'project vault is unreadable' }, { status: 500 })
          : HttpResponse.json([{ name: 'API_KEY', secret_name: 'API_KEY', is_default: false }]),
      ),
    );

    render(
      <AppProviders>
        <SecretsContent shouldCreate={false} search="" onSearchChange={noop} />
      </AppProviders>,
    );

    const errorState = await screen.findByTestId(SECRETS_LOAD_ERROR_TESTID, undefined, { timeout: 15_000 });
    expect(within(errorState).getByText('Failed to load secrets')).toBeInTheDocument();
    expect(screen.queryByText('No secrets')).not.toBeInTheDocument();
    expect(screen.queryByRole('grid')).not.toBeInTheDocument();

    failing = false;
    fireEvent.click(within(errorState).getByRole('button', { name: 'Try again' }));

    await waitFor(() => {
      expect(screen.getAllByText('API_KEY').length).toBeGreaterThan(0);
    });
    expect(screen.queryByTestId(SECRETS_LOAD_ERROR_TESTID)).not.toBeInTheDocument();
  }, 30_000);

  it('offers no retry for a 403 — retrying cannot help', async () => {
    useSelectedProjectStore.setState({ project: { id: 'proj-1', name: 'Acme' } });
    server.use(
      http.get(PERMISSIONS_PATH, () =>
        HttpResponse.json([{ name: 'configuration.secrets.secret.list', enabled: true }]),
      ),
      http.get(SECRETS_PATH, () => HttpResponse.json({ error: 'forbidden' }, { status: 403 })),
    );

    render(
      <AppProviders>
        <SecretsContent shouldCreate={false} search="" onSearchChange={noop} />
      </AppProviders>,
    );

    const errorState = await screen.findByTestId(SECRETS_LOAD_ERROR_TESTID, undefined, { timeout: 15_000 });
    expect(within(errorState).getByText('The access is not allowed')).toBeInTheDocument();
    expect(within(errorState).queryByRole('button')).not.toBeInTheDocument();
    expect(screen.queryByText('No secrets')).not.toBeInTheDocument();
  }, 30_000);
});
