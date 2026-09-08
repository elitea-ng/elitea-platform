/**
 * THE DISCRIMINATING TEST for the toolkit save-time credential refusal (#613).
 *
 * The server now answers a save whose credential reference does not resolve
 * with `400 {settings_errors: [{loc: ["settings", "<field>"], msg}]}`. Nothing
 * on this side consumed that: `ToolkitForm` has always held the effect that
 * turns `settings_errors` into per-field errors, and NO production render site
 * ever supplied the `toolkitValidation` prop it reads
 * (`git log -S toolkitValidation` over all three sites returns nothing).
 *
 * WHY NOT ToolkitForm.test.tsx / ToolkitForm.configuration.hooks.test.tsx.
 * Every existing test for this channel hands the component the prop itself
 * (`ToolkitForm.test.tsx`'s `const toolkitValidation: ToolkitValidationInjected
 * = {...}`), which is exactly why they all passed while production supplied
 * nothing. A test that injects the seam cannot see a missing composition root.
 *
 * This test injects NOTHING but HTTP. It renders the real `CreateToolkit` page
 * against a mocked 400 and asserts the server's sentence lands ON the
 * credential field — not in the page's generic "Failed to create the toolkit."
 * banner, which is what the whole body used to collapse into.
 *
 * VERIFIED to discriminate: with `toolkitValidation={toolkitValidation}`
 * removed from `CreateToolkit.tsx`'s `<ToolkitForm>` element, both tests below
 * fail (the message never appears; the generic banner appears instead), while
 * `ToolkitForm.test.tsx` and every case in
 * `ToolkitForm.configuration.hooks.test.tsx` stay green. Recorded output is in
 * the PR body.
 */
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

// NO `deps` override anywhere below: the page must reach its OWN
// `useToolkitCreate()` mutation, so the assertions run against the real
// generated `POST /elitea_core/tools/prompt_lib/{projectId}` and the real
// `EliteaApiError` a 400 produces. An injected `vi.fn()` would resolve with
// `undefined` and every failure here would be a local TypeError wearing the
// server's clothes.
import { CreateToolkit } from '../CreateToolkit';
import { renderToolkitsRoute } from './testRouter';

const TOOLKIT_LIST_URL = '/api/v2/elitea_core/tools/prompt_lib/:projectId';
const TOOLKIT_TYPES_URL = '/api/v2/elitea_core/toolkits/prompt_lib/:projectId';
const CONFIGURATIONS_LIST_URL = '/api/v2/configurations/configurations/:projectId';
const CONFIGURATIONS_AVAILABLE_URL = '/api/v2/configurations/available/';
/**
 * The SAVED-row batch check, not the candidate-payload one. The picker moved to
 * it because every stored `data` object carries a `{{secret.NAME}}` reference
 * rather than the token (`secret_sealing.go`), so the candidate route could only
 * ever ask the provider to authenticate a template string.
 */
const CHECK_CONNECTIONS_URL = '/api/v2/configurations/check_stored_connections/:projectId';
const MODELS_URL = '/api/v2/configurations/models/:projectId';

/** The same served shape `credentialPickerWiring.test.tsx` pins: a `$defs` entry the property `$ref`s, which is what makes the field a credential field at all. */
const GITHUB_TYPE_SCHEMA = {
  title: 'github',
  type: 'object',
  metadata: { label: 'GitHub' },
  $defs: {
    github: { type: 'object', metadata: { section: 'credentials', type: 'github' } },
  },
  properties: {
    github_configuration: { $ref: '#/$defs/github', configuration_types: ['github'] },
    selected_tools: { args_schemas: { search_code: { type: 'object' } } },
  },
};

const SAVED_CREDENTIAL = {
  uuid: 'cfg-1',
  id: 'cfg-1',
  type: 'github',
  elitea_title: 'ci-bot',
  label: 'CI Bot Token',
  project_id: 'proj-1',
  section: 'credentials',
  data: { base_url: 'https://api.github.com' },
};

/** The exact sentence `services/elitea-main/internal/api/v2/toolkits/settings_validation.go` composes for `configuration_not_found`. */
const REFUSAL_MESSAGE = 'Your configuration does not match any available configurations.';

const GENERIC_BANNER = 'Failed to create the toolkit.';

interface CreateOutcome {
  readonly status: number;
  readonly body: Record<string, unknown>;
}

function mockEndpoints(create: CreateOutcome): { readonly creates: () => number } {
  let creates = 0;
  server.use(
    http.get(TOOLKIT_LIST_URL, () => HttpResponse.json({ rows: [], total: 0 })),
    http.get(TOOLKIT_TYPES_URL, () => HttpResponse.json({ github: GITHUB_TYPE_SCHEMA })),
    http.get(CONFIGURATIONS_LIST_URL, () =>
      HttpResponse.json({ items: [SAVED_CREDENTIAL], total: 1, limit: 500, offset: 0, shared: { items: [], total: 0 } }),
    ),
    http.get(CONFIGURATIONS_AVAILABLE_URL, () => HttpResponse.json([])),
    http.post(CHECK_CONNECTIONS_URL, () => HttpResponse.json([])),
    http.get(MODELS_URL, () => HttpResponse.json({ items: [], total: 0 })),
    http.post(TOOLKIT_LIST_URL, () => {
      creates += 1;
      return HttpResponse.json(create.body, { status: create.status });
    }),
  );
  return { creates: () => creates };
}

if (typeof Element.prototype.scrollIntoView !== 'function') {
  Element.prototype.scrollIntoView = () => undefined;
}

/**
 * Reach a save-ready create form: pick the type, pick the credential. The name
 * field is deliberately left blank — this page gates Save on nothing local (see
 * `CreateToolkit.tsx`'s disclosed gap), and typing into it only lengthens the
 * test.
 */
async function fillCreateForm(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.click(await screen.findByText('GitHub'));
  await user.click(await screen.findByRole('combobox', { name: /Github Configuration/i }));
  await user.click(within(await screen.findByRole('listbox')).getByText('CI Bot Token'));
  await waitFor(() => {
    expect(screen.getByRole('combobox', { name: /Github Configuration/i })).toHaveTextContent('CI Bot Token');
  });
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('a refused toolkit save lands on the field the server named', () => {
  it('shows the server sentence beside the credential field instead of the generic banner', async () => {
    mockEndpoints({
      status: 400,
      body: {
        error: 'toolkit settings reference a configuration that is not available in this project',
        valid: false,
        settings_errors: [{ loc: ['settings', 'github_configuration'], msg: REFUSAL_MESSAGE, type: 'value_error', code: 'configuration_not_found' }],
      },
    });
    const user = userEvent.setup();

    renderToolkitsRoute(<CreateToolkit />, '/toolkits/create', { projectId: 'proj-1' });

    await fillCreateForm(user);
    await user.click(await screen.findByRole('button', { name: /^Save$/i }));

    // The refusal reached the FIELD. Without a `toolkitValidation` supplier the
    // 400's body is discarded and nothing but the banner below ever renders.
    expect(await screen.findByText(REFUSAL_MESSAGE)).toBeInTheDocument();
    expect(screen.queryByText(GENERIC_BANNER)).not.toBeInTheDocument();
  }, 30_000);

  it('does not re-issue the same refused request on a second Save', async () => {
    const tracker = mockEndpoints({
      status: 400,
      body: {
        valid: false,
        settings_errors: [{ loc: ['settings', 'github_configuration'], msg: REFUSAL_MESSAGE, type: 'value_error', code: 'configuration_not_found' }],
      },
    });
    const user = userEvent.setup();

    renderToolkitsRoute(<CreateToolkit />, '/toolkits/create', { projectId: 'proj-1' });

    await fillCreateForm(user);
    const save = await screen.findByRole('button', { name: /^Save$/i });
    await user.click(save);
    await screen.findByText(REFUSAL_MESSAGE);

    await user.click(save);
    await waitFor(() => {
      expect(tracker.creates()).toBe(1);
    });
  }, 30_000);

  /*
   * The other direction. A rejection that carries no field-keyed body — a 500,
   * a dropped connection — still has to reach the user, and the only place it
   * can go is the page's own banner. A supplier that swallowed everything would
   * pass the first test and fail here.
   */
  it('still shows the generic banner for a failure that names no field', async () => {
    mockEndpoints({ status: 500, body: { error: 'boom' } });
    const user = userEvent.setup();

    renderToolkitsRoute(<CreateToolkit />, '/toolkits/create', { projectId: 'proj-1' });

    await fillCreateForm(user);
    await user.click(await screen.findByRole('button', { name: /^Save$/i }));

    expect(await screen.findByText(GENERIC_BANNER)).toBeInTheDocument();
    expect(screen.queryByText(REFUSAL_MESSAGE)).not.toBeInTheDocument();
  }, 30_000);

  /*
   * THE SHAPE-DRIFT CASE, and the one that used to fail silently.
   *
   * `useToolkitSaveValidation.reportSaveError` returned `true` on the mere
   * PRESENCE of a non-empty `settings_errors` array, and `CreateToolkit`
   * suppresses its banner on that `true`. But `parseValidationErrors` drops
   * every entry whose `loc[1]` is missing, so a refusal shaped like the one
   * below painted NO field error and NO banner — a completely silent failed
   * save — and the page's `if (toolkitValidation.isError) return` guard then
   * latched Save shut until the user happened to edit something.
   *
   * The shape is not hypothetical: it is what this handler's OTHER
   * `settings_errors` emitter, the `/toolkit_validator/` route, sends today.
   *
   * Both assertions are needed. The banner proves the refusal is visible at
   * all; the second Save proves the page is not wedged.
   */
  it('shows the generic banner, and leaves Save usable, when settings_errors cannot be keyed to a field', async () => {
    const tracker = mockEndpoints({
      status: 400,
      body: {
        valid: false,
        // A ONE-element `loc`: `locFieldKey` reads `loc[1]` and finds nothing.
        settings_errors: [{ loc: ['settings'], msg: REFUSAL_MESSAGE, type: 'value_error' }],
      },
    });
    const user = userEvent.setup();

    renderToolkitsRoute(<CreateToolkit />, '/toolkits/create', { projectId: 'proj-1' });

    await fillCreateForm(user);
    const save = await screen.findByRole('button', { name: /^Save$/i });
    await user.click(save);

    expect(await screen.findByText(GENERIC_BANNER)).toBeInTheDocument();

    await user.click(save);
    await waitFor(() => {
      expect(tracker.creates()).toBe(2);
    });
  }, 30_000);
});
