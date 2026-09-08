/**
 * THE DISCRIMINATING TESTS for the toolkit credential picker (#308).
 *
 * Every existing test for this surface renders the consumer WITH the slot
 * handed to it — `ToolBaseProperty.test.tsx` passes its own
 * `renderCredentialLikeField`, `IndexScheduleModal.test.tsx` passes its own
 * `renderCredentialsSelect`. All of them passed while production supplied
 * neither, because the composition root is the thing that was missing, and a
 * test that injects the slot cannot see that.
 *
 * These tests inject NOTHING. They render the real `EditToolkit` and
 * `CreateToolkit` PAGES against mocked HTTP only, so each one fails if any hop
 * stops forwarding:
 *
 *   EditToolkit -> ConfigurationTab(slots.renderCredentialPicker)
 *     -> ToolkitForm(slots) -> useCredentialLikeFieldSlot
 *     -> ToolBaseProperty.dispatch -> CredentialsSelect
 *
 *   EditToolkit -> IndexesTab(renderCredentialsSelect) -> IndexesContainer
 *     -> IndexDetails -> IndexActions -> IndexScheduleModal -> CredentialsSelect
 *
 * VERIFIED to discriminate: with `renderCredentialPicker` removed from
 * `EditToolkit.tsx`'s and `CreateToolkit.tsx`'s slot objects, the first three
 * tests fail; with `renderCredentialsSelect` removed from `EditToolkit.tsx`,
 * the last one fails. See the PR body for the recorded failure output.
 *
 * The served schema shape below is the one PR #352 (issue #330) makes the Go
 * catalogue serve: a `$defs` block whose entry carries
 * `metadata.section`, and a property that `$ref`s it. That reference is what
 * `toolkitSchema.helpers.ts`'s `findConfigDefKey` keys the `configuration`
 * kind off. A property WITHOUT it is not a credential field at all, which is
 * what the negative test below pins.
 */
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { CreateToolkit } from '../CreateToolkit';
import { EditToolkit } from '../EditToolkit';
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
/** The single-row form of the stored check — what the option row's Reload control calls. */
const CHECK_CONNECTION_URL = '/api/v2/configurations/check_stored_connection/:projectId/:configId';
const INDEX_META_URL = '/api/v2/elitea_core/index_meta/prompt_lib/:projectId/:toolkitId';
const MODELS_URL = '/api/v2/configurations/models/:projectId';

/** The exact served shape from PR #352: one `$defs` entry, one `$ref` property, `metadata.section: "credentials"`. */
const GITHUB_TYPE_SCHEMA = {
  title: 'github',
  type: 'object',
  metadata: { label: 'GitHub' },
  $defs: {
    github: { type: 'object', metadata: { section: 'credentials', type: 'github' } },
  },
  properties: {
    github_configuration: { $ref: '#/$defs/github', configuration_types: ['github'] },
    selected_tools: { args_schemas: { index_data: { type: 'object' }, search_code: { type: 'object' } } },
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

function mockToolkitRow(overrides: Partial<Record<string, unknown>> = {}) {
  return {
    id: 'tk-1',
    type: 'github',
    name: 'My GitHub',
    description: '',
    settings: { selected_tools: ['index_data'] },
    meta: {},
    created_at: '2026-01-01T00:00:00Z',
    author_id: 1,
    ...overrides,
  };
}

interface EndpointOptions {
  readonly typeSchema?: Record<string, unknown>;
  readonly credentials?: readonly Record<string, unknown>[];
  readonly toolkitRow?: Record<string, unknown>;
  readonly indexes?: readonly Record<string, unknown>[];
  /** The batch stored-check rows the server answers with. Default: none checked. */
  readonly storedCheckRows?: readonly Record<string, unknown>[];
}

function mockEndpoints(options: EndpointOptions = {}): void {
  const credentials = options.credentials ?? [SAVED_CREDENTIAL];
  server.use(
    http.get(TOOLKIT_LIST_URL, () => HttpResponse.json({ rows: [options.toolkitRow ?? mockToolkitRow()], total: 1 })),
    http.get(TOOLKIT_TYPES_URL, () => HttpResponse.json({ github: options.typeSchema ?? GITHUB_TYPE_SCHEMA })),
    http.get(CONFIGURATIONS_LIST_URL, () =>
      HttpResponse.json({ items: credentials, total: credentials.length, limit: 500, offset: 0, shared: { items: [], total: 0 } }),
    ),
    http.get(CONFIGURATIONS_AVAILABLE_URL, () => HttpResponse.json([])),
    http.post(CHECK_CONNECTIONS_URL, () => HttpResponse.json([...(options.storedCheckRows ?? [])])),
    http.get(MODELS_URL, () => HttpResponse.json({ items: [], total: 0 })),
    http.get(INDEX_META_URL, () => HttpResponse.json(options.indexes ?? [])),
  );
}

// jsdom implements no layout, so it ships no `scrollIntoView`. The index chat
// pane calls it on mount, and without this the whole Indexes tab falls into its
// error boundary. Same class of jsdom gap as `installCodeMirrorTestPolyfills`.
if (typeof Element.prototype.scrollIntoView !== 'function') {
  Element.prototype.scrollIntoView = () => undefined;
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('the toolkit credential picker is supplied by the real composition root', () => {
  it('renders the credential picker on the Edit page, with the project\'s real saved credential as an option', async () => {
    mockEndpoints();
    const user = userEvent.setup();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit: vi.fn() }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');

    // The picker's own label, derived by the form from the property key.
    const picker = await screen.findByRole('combobox', { name: /Github Configuration/i });

    await user.click(picker);

    // A REAL option, sourced from the mocked configurations endpoint. A slot
    // that renders nothing, or one wired to no data, has no listbox at all.
    const listbox = await screen.findByRole('listbox');
    expect(within(listbox).getByText('CI Bot Token')).toBeInTheDocument();
  });

  it('records the picked credential in the form, so the toolkit saves with it', async () => {
    mockEndpoints();
    const user = userEvent.setup();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit: vi.fn() }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    await user.click(await screen.findByRole('combobox', { name: /Github Configuration/i }));
    await user.click(within(await screen.findByRole('listbox')).getByText('CI Bot Token'));

    // The selection travelled OUT of the picker, through `onChange` ->
    // `editField` -> the form's own settings, and back IN as the picker's
    // value. A picker that renders but is not wired to `editField` shows the
    // placeholder here instead.
    await waitFor(() => {
      expect(screen.getByRole('combobox', { name: /Github Configuration/i })).toHaveTextContent('CI Bot Token');
    });
  });

  it('renders the credential picker on the Create page too', async () => {
    mockEndpoints();
    const user = userEvent.setup();

    renderToolkitsRoute(<CreateToolkit deps={{ createToolkit: vi.fn() }} />, '/toolkits/create', { projectId: 'proj-1' });

    // Pick the type first — the form appears only once a type is chosen.
    await user.click(await screen.findByText('GitHub'));

    expect(await screen.findByRole('combobox', { name: /Github Configuration/i })).toBeInTheDocument();
  });

  /*
   * The negative half. Without the `$defs` block PR #352 adds, the property is
   * an ordinary one and NO credential picker belongs on the page. This pins
   * that the picker follows the served reference, not the property's name — so
   * a future change that starts matching on `*_configuration` by name, or one
   * that renders the picker unconditionally, fails here.
   */
  it('renders NO credential picker when the served property carries no $defs reference', async () => {
    mockEndpoints({
      typeSchema: {
        title: 'github',
        type: 'object',
        metadata: { label: 'GitHub' },
        properties: { github_configuration: { type: 'object' } },
      },
    });

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit: vi.fn() }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    await waitFor(() => {
      expect(screen.queryByRole('combobox', { name: /Github Configuration/i })).not.toBeInTheDocument();
    });
  });
});

describe('the index schedule modal credential select is supplied by the real composition root', () => {
  it('renders a labelled credential select in the schedule modal, with the real saved credential', async () => {
    mockEndpoints({
      // `indexed` is what `IndexesContainer`'s auto-select effect looks for,
      // and `completed` is a `RUNNABLE_INDEX_STATUSES` member — without both,
      // the schedule control stays disabled and the modal never opens.
      indexes: [
        {
          id: 'idx-1',
          metadata: { collection: 'Repo docs', state: 'completed', indexed: '2026-01-01T00:00:00Z' },
        },
      ],
    });
    const user = userEvent.setup();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit: vi.fn() }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    await user.click(await screen.findByRole('tab', { name: 'Indexes' }));
    await user.click(await screen.findByTestId('index-schedule-settings'));

    const dialog = await screen.findByRole('dialog');

    // The label is the #308 decision under test: the served property carries no
    // `description`, so the modal derives it from the property key. An
    // unlabelled picker is the state this issue forbids shipping.
    const select = await within(dialog).findByRole('combobox', { name: /Github Configuration/i });

    await user.click(select);
    expect(within(await screen.findByRole('listbox')).getByText('CI Bot Token')).toBeInTheDocument();
  });
});

/*
 * ── THE CREDENTIAL STATUS INDICATOR (#319, toolkit half) ────────────────────
 *
 * The attention indicator on a credential row is driven by the stored-check
 * routes. Until `internal/api/v2/configurations/toolkit_check.go` existed those
 * routes answered "Checking connection is not supported yet for configuration
 * type github" for EVERY toolkit credential, which `useCredentialValidation`
 * maps to `unsupported` — so the indicator could not light for any toolkit
 * credential, right or wrong. These pin both directions.
 */
describe('the credential status indicator reads the real stored connection check', () => {
  it('marks a credential the provider refused, with the reason on the control', async () => {
    mockEndpoints({
      storedCheckRows: [
        { id: 'cfg-1', success: false, reason: 'auth_failed', message: 'Authentication failed. The provider rejected this credential.' },
      ],
    });
    const user = userEvent.setup();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit: vi.fn() }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    await user.click(await screen.findByRole('combobox', { name: /Github Configuration/i }));

    const indicator = await screen.findByTestId('credential-status-indicator');
    expect(indicator).toHaveAttribute('aria-label', expect.stringMatching(/Authentication failed/i));
    expect(await screen.findByTestId('credential-reload-button')).toBeInTheDocument();
  });

  it('clears the indicator when a re-check finds the credential works, without a page reload', async () => {
    mockEndpoints({
      storedCheckRows: [
        { id: 'cfg-1', success: false, reason: 'auth_failed', message: 'Authentication failed. The provider rejected this credential.' },
      ],
    });
    // The re-check is the SAVED-row route with no body: the browser never held
    // the token, so this is the only request that can answer the question.
    let recheckBody: string | undefined;
    server.use(
      http.post(CHECK_CONNECTION_URL, async ({ request }) => {
        recheckBody = await request.text();
        return HttpResponse.json({ success: true, reason: 'ok', message: 'Connection successful' });
      }),
    );
    const user = userEvent.setup();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit: vi.fn() }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    await user.click(await screen.findByRole('combobox', { name: /Github Configuration/i }));
    await user.click(await screen.findByTestId('credential-reload-button'));

    await waitFor(() => {
      expect(screen.queryByTestId('credential-status-indicator')).not.toBeInTheDocument();
    });
    expect(screen.queryByTestId('credential-reload-button')).not.toBeInTheDocument();
    expect(recheckBody, 'the stored check must send no credential — the browser does not hold one').toBe('');
  });

  it('does NOT mark a credential whose type this build carries no probe for', async () => {
    // `unsupported_type` is not evidence that the credential is broken, and the
    // attention indicator means exactly that.
    mockEndpoints({
      storedCheckRows: [
        { id: 'cfg-1', success: false, reason: 'unsupported_type', message: 'Checking connection is not supported yet for configuration type github' },
      ],
    });
    const user = userEvent.setup();

    renderToolkitsRoute(<EditToolkit deps={{ saveToolkit: vi.fn() }} />, '/toolkits/latest/tk-1', { projectId: 'proj-1' });

    await screen.findByText('My GitHub');
    await user.click(await screen.findByRole('combobox', { name: /Github Configuration/i }));
    await screen.findByText('CI Bot Token');

    expect(screen.queryByTestId('credential-status-indicator')).not.toBeInTheDocument();
  });
});
