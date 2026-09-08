/**
 * Behavioural coverage for the credentials/configuration family's ROUTE-OWNED
 * navigation callbacks and for `configurationMode` threading — the last gap
 * left by the Phase-2 route-wiring pass (PRs #98..#108).
 *
 * Why this file exists: five routes hand their page a set of callbacks
 * (`onSelectCredential`/`onCreateNew`/`onSaved`/`onDiscarded`/`onCreated`/
 * `onCancelled`) and, for two of them, a `configurationMode` flag. Before
 * this suite, nothing asserted where those callbacks actually navigate, and
 * nothing asserted that the flag differs between the `/credentials/*` and
 * `/settings/*-configuration` mounts of the SAME page component. A route
 * could have pointed every callback at `/` — or dropped `configurationMode`
 * entirely — and every existing test (including the E2E journeys and the
 * route-wiring map gate, which only prove a route imports a component) would
 * still have been green.
 *
 *   /credentials/:tab                            (ROUTE-022)
 *   /credentials/:tab/:credential_uid            (ROUTE-025, configurationMode OFF)
 *   /credentials/create-credential               (ROUTE-023, configurationMode OFF)
 *   /settings/create-configuration               (ROUTE-063, configurationMode ON)
 *   /settings/edit-configuration/:credential_uid (ROUTE-065, configurationMode ON)
 *
 * Everything here runs against the REAL generated route tree through a REAL
 * `RouterProvider` (§6.2 — the router is never mocked), mounted the same way
 * `settingsLayout.test.tsx` does it: memory history + QueryClientProvider +
 * ThemeProvider. Assertions read `router.state.location.pathname` after
 * driving the page's own controls, so they fail if a callback is rewired,
 * dropped, or pointed somewhere else — not merely if a prop name disappears.
 *
 * Landmark scoping: every content assertion is made INSIDE `role="main"`
 * (`widgets/app-shell`'s own landmark). The sidebar renders a "Credential"
 * nav item and the settings drawer renders "AI Configuration"/"Secrets" on
 * every route in their families, so an unscoped text query would pass
 * without the page under test rendering at all.
 *
 * ROUTE-024/064 (`/credentials/create-credential/:credentialType` and
 * `/settings/create-configuration/:credentialType`) are empty pattern-A
 * children, so their parents own the param in BOTH directions — reading it
 * on entry and writing it when a type is picked. Both directions are
 * asserted below; that param is what decides form-vs-picker.
 *
 * NOT covered here (deliberate, and not implied by a green run):
 *  - The delete path (`CredentialsControls` -> `onDiscarded`) — same
 *    callback, already driven here through Discard.
 *  - `from`/`forceCustom` on these routes. `prefill_name`/`prefill_id`/
 *    `section` ARE covered below, since the create routes now thread them;
 *    `searchParams.credentials.test.tsx` covers only their VALIDATION, not
 *    their effect on the page.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { createMemoryHistory, createRouter, RouterProvider } from '@tanstack/react-router';
import CssBaseline from '@mui/material/CssBaseline';
import { ThemeProvider } from '@mui/material/styles';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { AuthContext } from '@/app/router-context';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';
import { resetConfigForTests } from '@/shared/config/get-config';
import { useSelectedProjectStore } from '@/widgets/app-shell';

import { routeTree } from '../../routeTree.gen';
import { server } from '../../test/setup';

const PROJECT_ID = '7';
const CREDENTIAL_UID = 'cred-9';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

/**
 * `personal_project_id` deliberately differs from the selected project, so
 * `useCredentialFormContext` derives `isTeamProject: true` — the branch a
 * real team project takes.
 */
const auth: AuthContext = {
  getUser: () => ({ id: 'u1', personal_project_id: 'personal-1', permissions: [], publicPermissions: [] }),
  getSelectedProjectId: () => PROJECT_ID,
};

/** One credential type, enough for the type selector to offer a tile and for the form to resolve a schema. */
const TYPE_DESCRIPTOR = {
  type: 'openai',
  config_schema: { title: 'OpenAI', metadata: { label: 'OpenAI' }, properties: { data: { properties: {} } } },
};

/**
 * jsdom has no `ResizeObserver`; `CategoryItemCard` (every tile in the
 * credential-type selector) mounts one through `useTextOverflow` and would
 * otherwise throw into the route's error boundary. Same stub shape the
 * chat-input suites already use.
 */
class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

interface SaveSpies {
  readonly created: () => boolean;
  readonly updated: () => boolean;
}

/**
 * The `/configurations/*` endpoints are hand-registered (no OpenAPI schema,
 * so the generated MSW registry does not cover them) — every one the screens
 * under test touch has to be stubbed explicitly, or `onUnhandledRequest:
 * 'error'` fires. Wildcard-prefixed because the configured client resolves
 * against an absolute origin at request time.
 */
function installHandlers(): SaveSpies {
  let created = false;
  let updated = false;
  server.use(
    http.get(`*/auth/permissions/prompt_lib/${PROJECT_ID}`, () =>
      HttpResponse.json([
        { name: 'configurations.configuration.update', enabled: true },
        { name: 'configurations.configuration.delete', enabled: true },
      ]),
    ),
    http.get('*/configurations/available/', () => HttpResponse.json([TYPE_DESCRIPTOR])),
    http.get(`*/configurations/configurations/${PROJECT_ID}`, () =>
      HttpResponse.json({
        items: [{ uid: CREDENTIAL_UID, type: 'openai', elitea_title: 'A cred', label: 'A cred' }],
        total: 1,
        limit: 20,
        offset: 0,
      }),
    ),
    http.post(`*/configurations/configurations/${PROJECT_ID}`, () => {
      created = true;
      // `id` (not just `uid`) is what `performSave` reads to build the
      // `?reveal=` search param `leave` navigates with — see
      // `useCredentialFormController.ts`'s `onSaved` doc comment.
      return HttpResponse.json({ id: 'cfg-new-1', uid: 'cred-new' });
    }),
    http.post(`*/configurations/check_connections/${PROJECT_ID}`, () => HttpResponse.json([])),
    http.get(`*/configurations/configuration/${PROJECT_ID}/${CREDENTIAL_UID}`, () =>
      HttpResponse.json({ uid: CREDENTIAL_UID, type: 'openai', elitea_title: 'A cred', label: 'A cred', data: {} }),
    ),
    http.put(`*/configurations/configuration/${PROJECT_ID}/${CREDENTIAL_UID}`, () => {
      updated = true;
      return HttpResponse.json({ id: CREDENTIAL_UID, uid: CREDENTIAL_UID });
    }),
    // Shell chrome, not the screens under test: neither endpoint is in the
    // generated MSW registry, and `onUnhandledRequest: 'error'` would log a
    // failure for each on every mount.
    http.get('*/notifications/notifications/prompt_lib/*', () => HttpResponse.json({ rows: [], total: 0 })),
    http.get('*/social/author', () => HttpResponse.json({})),
  );
  return { created: () => created, updated: () => updated };
}

function mountAt(path: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const history = createMemoryHistory({ initialEntries: [path] });
  const router = createRouter({ routeTree, history, context: { auth } });
  render(
    <QueryClientProvider client={queryClient}>
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <CssBaseline />
        <RouterProvider router={router} />
      </ThemeProvider>
    </QueryClientProvider>,
  );
  return router;
}

/**
 * The app-shell content landmark — everything the route under test renders
 * lives inside it, and the sidebar (which carries a "Credential" nav item on
 * every route) does not.
 *
 * Deliberately the OUTERMOST `<main>`: `settings-layout.tsx` nests a second
 * one inside the shell's, so `getByRole('main')` is ambiguous on the
 * `/settings/*` routes. The outer element is the one that does the scoping
 * work here; the inner one is a strict subset of it.
 */
async function main() {
  const [outermost] = await screen.findAllByRole('main');
  if (outermost === undefined) throw new Error('app-shell <main> landmark not found');
  return within(outermost);
}

/**
 * `DiscardButton` confirms through a modal before firing, and in EDIT mode
 * its trigger and the modal's confirm button share the label "Discard" —
 * so the confirm click is resolved inside the dialog, never by index.
 */
async function discardVia(triggerName: 'Discard' | 'Cancel'): Promise<void> {
  fireEvent.click(await (await main()).findByRole('button', { name: triggerName }));
  const dialog = await screen.findByRole('dialog');
  fireEvent.click(within(dialog).getByRole('button', { name: 'Discard' }));
}

/** The create screen shows a type selector first; the form (and its Save/Cancel pair) only exists once a type is picked. */
async function chooseTypeAndName(name: string): Promise<void> {
  fireEvent.click(await (await main()).findByRole('button', { name: 'OpenAI' }));
  fireEvent.change(await (await main()).findByLabelText('Name'), { target: { value: name } });
}

beforeEach(() => {
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
  resetConfigForTests();
  vi.stubEnv('VITE_SERVER_URL', 'https://elitea.example');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', '11');
  configureGeneratedClient({ baseUrl: '/api/v2' });
  useSelectedProjectStore.setState({ project: { id: PROJECT_ID, name: 'Team project' } });
});

afterEach(() => {
  useSelectedProjectStore.setState({ project: null });
  resetGeneratedClient();
  resetConfigForTests();
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
});

describe('ROUTE-022 /credentials/:tab navigation callbacks', () => {
  it('onSelectCredential opens the clicked credential under the CURRENT tab', async () => {
    installHandlers();
    const router = mountAt('/credentials/all');

    fireEvent.click(await (await main()).findByText('A cred'));

    await waitFor(() => {
      expect(router.state.location.pathname).toBe(`/credentials/all/${CREDENTIAL_UID}`);
    });
  });

  it('onSelectCredential preserves a non-default :tab rather than hard-coding one', async () => {
    installHandlers();
    const router = mountAt('/credentials/mine');

    fireEvent.click(await (await main()).findByText('A cred'));

    await waitFor(() => {
      expect(router.state.location.pathname).toBe(`/credentials/mine/${CREDENTIAL_UID}`);
    });
  });

  it('onCreateNew goes to the create-credential screen', async () => {
    installHandlers();
    const router = mountAt('/credentials/all');

    fireEvent.click(await (await main()).findByRole('button', { name: 'New credential' }));

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/credentials/create-credential');
    });
  });
});

describe('ROUTE-025 /credentials/:tab/:credential_uid navigation callbacks', () => {
  it('onDiscarded returns to the tab the editor was opened from', async () => {
    installHandlers();
    const router = mountAt(`/credentials/mine/${CREDENTIAL_UID}`);

    await discardVia('Discard');

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/credentials/mine');
    });
  });

  it('onSaved returns to the same tab after a successful update', async () => {
    const spies = installHandlers();
    const router = mountAt(`/credentials/mine/${CREDENTIAL_UID}`);

    fireEvent.click(await (await main()).findByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(spies.updated()).toBe(true);
    });
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/credentials/mine');
    });
  });

  it('renders the page WITHOUT configurationMode — the editor is titled "Credential"', async () => {
    installHandlers();
    mountAt(`/credentials/all/${CREDENTIAL_UID}`);

    const scope = await main();
    expect(await scope.findByText('Credential')).toBeInTheDocument();
    expect(scope.queryByText('Configuration')).not.toBeInTheDocument();
  });
});

describe('ROUTE-023 /credentials/create-credential navigation callbacks', () => {
  it('onCancelled returns to the credentials list (via the /credentials index redirect)', async () => {
    installHandlers();
    const router = mountAt('/credentials/create-credential');

    await chooseTypeAndName('new cred');
    await discardVia('Cancel');

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/credentials/all');
    });
  });

  it('onCreated returns to the credentials list after a successful create', async () => {
    const spies = installHandlers();
    const router = mountAt('/credentials/create-credential');

    await chooseTypeAndName('new cred');
    fireEvent.click(await (await main()).findByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(spies.created()).toBe(true);
    });
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/credentials/all');
    });
  });

  it('renders the page WITHOUT configurationMode — the form is titled "Credential"', async () => {
    installHandlers();
    mountAt('/credentials/create-credential');

    await chooseTypeAndName('new cred');

    const scope = await main();
    expect(await scope.findByText('Credential')).toBeInTheDocument();
    expect(scope.queryByText('Configuration')).not.toBeInTheDocument();
  });
});

/**
 * ROUTE-024/064 — the `:credentialType` deep link.
 *
 * Both are empty pattern-A children with no component: their parent renders
 * the screen and reads the param. In the baseline that param is what decides
 * FORM vs TYPE PICKER (`pages/Credentials/CreateCredential.jsx`'s `isEditing`,
 * which requires `useParams().credentialType`), and two producers link
 * straight at it — `CredentialWarningBanner.jsx:43` and
 * `useCredentialSearch.js:29`. So "does the picker get skipped" is the whole
 * behaviour of these two routes.
 *
 * The complementary "picker still shows when there is NO type segment" case
 * is not re-asserted here: every test above reaches the form through
 * `chooseTypeAndName`, which clicks a picker tile and would fail outright if
 * the parent ever pre-selected a type unconditionally.
 */
describe('ROUTE-024/064 :credentialType deep links skip the type picker', () => {
  it('/credentials/create-credential/:credentialType opens the form directly, titled "Credential"', async () => {
    installHandlers();
    mountAt('/credentials/create-credential/openai');

    const scope = await main();
    expect(await scope.findByLabelText('Name')).toBeInTheDocument();
    expect(scope.getByText('Credential')).toBeInTheDocument();
    expect(scope.queryByPlaceholderText('Search credentials')).not.toBeInTheDocument();
  });

  it('/settings/create-configuration/:credentialType opens the form directly, titled "Configuration"', async () => {
    installHandlers();
    mountAt('/settings/create-configuration/openai');

    const scope = await main();
    expect(await scope.findByLabelText('Name')).toBeInTheDocument();
    expect(scope.getByText('Configuration')).toBeInTheDocument();
    expect(scope.queryByPlaceholderText('Search credentials')).not.toBeInTheDocument();
  });

  it('the parent still owns navigation from the deep-linked URL (/credentials side)', async () => {
    installHandlers();
    const router = mountAt('/credentials/create-credential/openai');

    await (await main()).findByLabelText('Name');
    await discardVia('Cancel');

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/credentials/all');
    });
  });

  it('the parent still owns navigation from the deep-linked URL (/settings side)', async () => {
    installHandlers();
    const router = mountAt('/settings/create-configuration/openai');

    await (await main()).findByLabelText('Name');
    await discardVia('Cancel');

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/settings/model-configuration');
    });
  });

  it('an UNKNOWN type in the URL falls back to the picker instead of an empty form', async () => {
    installHandlers();
    mountAt('/credentials/create-credential/no-such-type');

    const scope = await main();
    // The picker's own search box — the form has a "Name" field and no such box.
    expect(await scope.findByPlaceholderText('Search credentials')).toBeInTheDocument();
    expect(scope.queryByLabelText('Name')).not.toBeInTheDocument();
  });
});

/**
 * The other half of the `:credentialType` contract: the URL is not just READ
 * on entry, it is WRITTEN when a type is picked. The baseline navigates on
 * selection (`hooks/credentials/useCredentialSearch.js:29`, which switches
 * between `CreateCredentialTypeFromMain` and `CreateConfigurationWithType`
 * depending on whether the user is inside settings) rather than holding the
 * choice in component state.
 *
 * That is what these assert, and it is why `useCredentialFormController` no
 * longer keeps a `selectedType`: with two sources of truth, picking a type
 * left the URL on the parent, and Back then dropped the param while the form
 * stayed on screen.
 */
describe('picking a type writes it to the URL', () => {
  it('/credentials/create-credential -> the picked type lands in the URL', async () => {
    installHandlers();
    const router = mountAt('/credentials/create-credential');

    fireEvent.click(await (await main()).findByRole('button', { name: 'OpenAI' }));

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/credentials/create-credential/openai');
    });
    expect(await (await main()).findByLabelText('Name')).toBeInTheDocument();
  });

  it('/settings/create-configuration -> the picked type stays in the SETTINGS route, not the credentials one', async () => {
    installHandlers();
    const router = mountAt('/settings/create-configuration');

    fireEvent.click(await (await main()).findByRole('button', { name: 'OpenAI' }));

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/settings/create-configuration/openai');
    });
    // Still the settings screen: configurationMode survives the hop.
    expect(await (await main()).findByText('Configuration')).toBeInTheDocument();
  });

  it('going back after picking a type returns to the picker, not a stale form', async () => {
    installHandlers();
    const router = mountAt('/credentials/create-credential');

    fireEvent.click(await (await main()).findByRole('button', { name: 'OpenAI' }));
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/credentials/create-credential/openai');
    });
    await (await main()).findByLabelText('Name');

    router.history.back();

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/credentials/create-credential');
    });
    const scope = await main();
    await waitFor(() => {
      expect(scope.queryByLabelText('Name')).not.toBeInTheDocument();
    });
    expect(scope.getByRole('button', { name: 'OpenAI' })).toBeInTheDocument();
  });
});

describe('ROUTE-063 /settings/create-configuration navigation callbacks', () => {
  it('onCancelled returns to the AI-configuration settings screen, NOT to /credentials', async () => {
    installHandlers();
    const router = mountAt('/settings/create-configuration');

    await chooseTypeAndName('new config');
    await discardVia('Cancel');

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/settings/model-configuration');
    });
  });

  it('onCreated returns to the AI-configuration settings screen after a successful create', async () => {
    const spies = installHandlers();
    const router = mountAt('/settings/create-configuration');

    await chooseTypeAndName('new config');
    fireEvent.click(await (await main()).findByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(spies.created()).toBe(true);
    });
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/settings/model-configuration');
    });
  });

  it('renders the SAME page WITH configurationMode — the form is titled "Configuration"', async () => {
    installHandlers();
    mountAt('/settings/create-configuration');

    await chooseTypeAndName('new config');

    const scope = await main();
    expect(await scope.findByText('Configuration')).toBeInTheDocument();
    expect(scope.queryByText('Credential')).not.toBeInTheDocument();
  });

  /**
   * DEFECT this pins (J19b). `leave` used to drop the saved row's id
   * entirely, so `ConfigurationsPanel` had no way to know which section (if
   * not LLMs) to open on return — see `ConfigurationsPanel.test.tsx`'s own
   * `reveal` coverage for the panel-level half of this fix.
   */
  it('onCreated carries the new row\'s id into ?reveal= on the way back', async () => {
    const spies = installHandlers();
    const router = mountAt('/settings/create-configuration');

    await chooseTypeAndName('new config');
    fireEvent.click(await (await main()).findByRole('button', { name: 'Save' }));

    await waitFor(() => expect(spies.created()).toBe(true));
    await waitFor(() => expect(router.state.location.pathname).toBe('/settings/model-configuration'));
    expect((router.state.location.search as { reveal?: string }).reveal).toBe('cfg-new-1');
  });

  it('onCancelled carries no ?reveal= — nothing was saved to reveal', async () => {
    installHandlers();
    const router = mountAt('/settings/create-configuration');

    await chooseTypeAndName('new config');
    await discardVia('Cancel');

    await waitFor(() => expect(router.state.location.pathname).toBe('/settings/model-configuration'));
    // No explicit `search` was passed to `navigate()` here (unlike the
    // `onCreated`/`onSaved` case above) — the destination's `reveal` schema
    // default is never applied, so the key is simply absent.
    expect((router.state.location.search as { reveal?: string }).reveal).toBeUndefined();
  });
});

describe('ROUTE-065 /settings/edit-configuration/:credential_uid navigation callbacks', () => {
  it('onDiscarded returns to the AI-configuration settings screen', async () => {
    installHandlers();
    const router = mountAt(`/settings/edit-configuration/${CREDENTIAL_UID}`);

    await discardVia('Discard');

    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/settings/model-configuration');
    });
  });

  it('onSaved returns to the AI-configuration settings screen after a successful update', async () => {
    const spies = installHandlers();
    const router = mountAt(`/settings/edit-configuration/${CREDENTIAL_UID}`);

    fireEvent.click(await (await main()).findByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(spies.updated()).toBe(true);
    });
    await waitFor(() => {
      expect(router.state.location.pathname).toBe('/settings/model-configuration');
    });
  });

  it('renders the SAME page WITH configurationMode — the editor is titled "Configuration"', async () => {
    installHandlers();
    mountAt(`/settings/edit-configuration/${CREDENTIAL_UID}`);

    const scope = await main();
    expect(await scope.findByText('Configuration')).toBeInTheDocument();
    expect(scope.queryByText('Credential')).not.toBeInTheDocument();
  });

  /** DEFECT this pins (J19b) — the edit path's half of the fix. */
  it('onSaved carries the edited row\'s id into ?reveal= on the way back', async () => {
    const spies = installHandlers();
    const router = mountAt(`/settings/edit-configuration/${CREDENTIAL_UID}`);

    fireEvent.click(await (await main()).findByRole('button', { name: 'Save' }));

    await waitFor(() => expect(spies.updated()).toBe(true));
    await waitFor(() => expect(router.state.location.pathname).toBe('/settings/model-configuration'));
    expect((router.state.location.search as { reveal?: string }).reveal).toBe(CREDENTIAL_UID);
  });

  it('onDiscarded carries no ?reveal= — the row was deleted, not saved', async () => {
    installHandlers();
    const router = mountAt(`/settings/edit-configuration/${CREDENTIAL_UID}`);

    await discardVia('Discard');

    await waitFor(() => expect(router.state.location.pathname).toBe('/settings/model-configuration'));
    // No explicit `search` was passed to `navigate()` for the discard path.
    expect((router.state.location.search as { reveal?: string }).reveal).toBeUndefined();
  });
});

/**
 * DEFECT: neither create route declared `prefill_name`/`prefill_id`/`section`
 * nor read them, so TanStack dropped all three. `CredentialWarningBanner`
 * builds `/credentials/create-credential/{type}?prefill_id=…&prefill_name=…
 * &section=…`. Every one of those deep links opened a form with an empty
 * name. The form read its type catalogue for EVERY section.
 *
 * Evidence at the time: a grep for `useSearch|prefillName|prefillId|section`
 * over both route files returned nothing. `CreateCredential`'s own header
 * called the props "resolved values" that nothing resolved.
 */
describe('the create routes thread their deep-link search params', () => {
  it('prefills the name from ?prefill_name', async () => {
    installHandlers();
    mountAt('/credentials/create-credential/openai?prefill_name=GitHub%20token');

    expect(await (await main()).findByLabelText('Name')).toHaveValue('GitHub token');
  });

  it('reads the type catalogue for the section the link names', async () => {
    installHandlers();
    const sections: string[][] = [];
    server.use(
      http.get('*/configurations/available/', ({ request }) => {
        sections.push(new URL(request.url).searchParams.getAll('section'));
        return HttpResponse.json([TYPE_DESCRIPTOR]);
      }),
    );

    mountAt('/credentials/create-credential?section=service_prompts');

    await waitFor(() => expect(sections).toContainEqual(['service_prompts']));
  });
});
