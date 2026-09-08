/**
 * The platform models panel.
 *
 * The assertions are the two states nothing else shows, plus the shape the
 * server owns:
 *
 *  1. **A model that does not resolve to a platform provider** — it names one
 *     the platform does not publish, or names none at all. Both fail provider
 *     admission and are served by nobody; this screen is the only place the
 *     reason is visible.
 *  2. **`status_ok = false`.** Stored, listed, never dispatched.
 *  3. **The credential select offers the server's list and nothing else.** It
 *     used to offer "None — infer from the model name" beside them, on the
 *     argument that the gateway resolves the provider from a prefix. That
 *     described no row this dialog can write: `ai_credentials` is required on
 *     all five model types, so the option produced a row listed here and served
 *     nowhere, and the server now answers 400 for it.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { PlatformModelsPanel } from './PlatformModelsPanel';
import { renderAdminRoute } from './__tests__/testRouter';

const MODEL_TYPES = [
  'asr_model',
  'embedding_model',
  'image_generation_model',
  'llm_model',
  'tts_model',
];

const GPT4O = {
  id: 11,
  uuid: 'uuid-11',
  elitea_title: 'gpt-4o',
  type: 'llm_model',
  section: 'llm',
  status_ok: true,
  status_logs: '',
  model_name: 'gpt-4o',
  credential_name: 'platform-openai',
  credential_resolves: true,
  low_tier: false,
  high_tier: false,
  created_at: '2026-08-01T00:00:00Z',
  updated_at: '2026-08-01T00:00:00Z',
};

function useModels(items: unknown[], extra: Record<string, unknown> = {}): void {
  server.use(
    http.get('*/admin/gateway/platform_models', () =>
      HttpResponse.json({
        items,
        total: items.length,
        public_project_id: 1,
        model_types: MODEL_TYPES,
        credential_names: ['platform-openai', 'platform-bedrock'],
        ...extra,
      }),
    ),
  );
}

/**
 * The admin project listing the grant picker reads.
 *
 * It is mounted ONLY for the "Selected projects" scope, so every test that does
 * not reach that scope needs no handler at all — `onUnhandledRequest: 'error'`
 * would fail them if the picker asked anyway, which is the check that keeps the
 * query where it belongs.
 */
function useProjects(rows: readonly { id: number; name: string }[]): void {
  server.use(
    http.get('*/admin/projects/administration', () =>
      HttpResponse.json({
        rows: rows.map((row) => ({
          ...row,
          owner_id: 1,
          owner_name: 'owner',
          admin_names: [],
          status: 'active',
          suspended: false,
          create_success: true,
          is_personal: false,
        })),
        total: rows.length,
        counts: { team: rows.length, personal: 0 },
      }),
    ),
  );
}

/** Fills in the three fields every save needs, on an open create dialog. */
async function fillNewModel(): Promise<void> {
  await userEvent.type(await screen.findByTestId('platform-model-name'), 'my-model');
  await userEvent.type(screen.getByTestId('platform-model-wire-name'), 'gpt-4o-mini');
  await userEvent.click(screen.getByRole('combobox', { name: /Platform provider/ }));
  await userEvent.click(await screen.findByRole('option', { name: 'platform-openai' }));
}

/** Captures the create bodies this panel posts. */
function captureCreates(bodies: unknown[]): void {
  server.use(
    http.post('*/admin/gateway/platform_models', async ({ request }) => {
      bodies.push(await request.json());
      return HttpResponse.json({ id: 12 }, { status: 201 });
    }),
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: 'https://elitea.example' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('PlatformModelsPanel', () => {
  it('lists the platform models with their provider', async () => {
    useModels([GPT4O]);
    renderAdminRoute(<PlatformModelsPanel />);

    const table = await screen.findByTestId('platform-models-table');
    expect(table).toHaveTextContent('gpt-4o');
    expect(table).toHaveTextContent('platform-openai');
    expect(table).toHaveTextContent('Chat / completion');
  });

  it('flags a model naming a provider the platform does not publish', async () => {
    useModels([{ ...GPT4O, credential_name: 'deleted-openai', credential_resolves: false }]);
    renderAdminRoute(<PlatformModelsPanel />);

    // The gateway serves this model anyway, with a provider guessed from the
    // model name. The wording has to say that, or an operator reads it as a
    // model that is simply broken and leaves it alone.
    const warning = await screen.findByTestId('platform-models-unresolved');
    expect(warning).toHaveTextContent('gpt-4o');
    expect(warning).toHaveTextContent('does not publish');
  });

  /*
   * A row written before the link became mandatory — by a seed, or by the
   * "None — infer from the model name" option this dialog used to offer. It
   * read "inferred from the name" and raised no warning, which described the
   * gateway's prefix fallback rather than the row: admission refuses an
   * unlinked model, so it is stored, listed here and served to nobody.
   */
  it('flags a legacy row that names no provider at all', async () => {
    useModels([{ ...GPT4O, credential_name: '', credential_resolves: false, status_ok: false }]);
    renderAdminRoute(<PlatformModelsPanel />);

    expect(await screen.findByTestId('platform-models-table')).toHaveTextContent(
      'no provider linked',
    );
    // Named in the alert, by its own model id: the table says WHICH column is
    // wrong, and the alert says the row is not being served.
    const warning = screen.getByTestId('platform-models-unresolved');
    expect(warning).toHaveTextContent('gpt-4o');
    expect(warning).toHaveTextContent('served to nobody');

    // Editing it is the repair path, and it opens on the state the row is in:
    // no provider chosen, so Save waits for one. A form that pre-selected the
    // first published provider would let one click attribute a model to a
    // credential nobody chose for it.
    await userEvent.click(screen.getByRole('button', { name: 'Edit' }));
    expect(await screen.findByTestId('platform-model-save')).toBeDisabled();
  });

  it('marks a model the gateway will not dispatch', async () => {
    useModels([{ ...GPT4O, status_ok: false }]);
    renderAdminRoute(<PlatformModelsPanel />);

    expect(await screen.findByText('Not resolving')).toBeVisible();
  });

  it('says why the provider list is empty when it could not be read', async () => {
    useModels([], { credential_names: [], credential_error: 'read platform credentials: timeout' });
    renderAdminRoute(<PlatformModelsPanel />);

    // Otherwise an empty provider list reads as "no providers are published",
    // which sends an operator to create a duplicate.
    expect(await screen.findByTestId('platform-models-credential-error')).toHaveTextContent(
      'timeout',
    );
  });

  it('does not claim there are no models when the read failed', async () => {
    server.use(
      http.get('*/admin/gateway/platform_models', () =>
        HttpResponse.json({ error: 'access_denied' }, { status: 403 }),
      ),
    );
    renderAdminRoute(<PlatformModelsPanel />);

    expect(await screen.findByTestId('platform-models-load-error')).toBeVisible();
    expect(screen.queryByTestId('platform-models-empty')).toBeNull();
  });

  it('offers the platform providers and nothing else', async () => {
    useModels([]);
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByTestId('platform-models-add'));
    // Addressed by its accessible NAME rather than by index: an index would
    // silently follow whichever select happened to be second if a field were
    // added above it.
    await userEvent.click(await screen.findByRole('combobox', { name: /Platform provider/ }));

    expect(await screen.findByRole('option', { name: 'platform-openai' })).toBeVisible();
    expect(screen.getByRole('option', { name: 'platform-bedrock' })).toBeVisible();
    // The option that made a row nothing serves. Asserted as an ABSENCE,
    // because its removal is the whole fix.
    expect(screen.queryByRole('option', { name: /None/ })).toBeNull();
    expect(screen.queryByRole('option', { name: /infer/i })).toBeNull();
  });

  it('will not save a model until a provider is chosen, and says why', async () => {
    useModels([]);
    const bodies: unknown[] = [];
    server.use(
      http.post('*/admin/gateway/platform_models', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ id: 12 }, { status: 201 });
      }),
    );
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByTestId('platform-models-add'));
    await userEvent.type(await screen.findByTestId('platform-model-name'), 'my-model');
    await userEvent.type(screen.getByTestId('platform-model-wire-name'), 'gpt-4o-mini');

    // Everything else is filled in, so the only thing holding Save is the one
    // the message names.
    // Disabled, not merely refused on submit: the operator is told which field
    // is missing while they can still fill it. `userEvent` cannot even click
    // it — MUI leaves a disabled button with `pointer-events: none` — so the
    // assertion is the disabled state itself, and nothing was posted.
    expect(screen.getByTestId('platform-model-save')).toBeDisabled();
    expect(screen.getByText(/served to nobody/)).toBeVisible();
    expect(bodies).toEqual([]);

    await userEvent.click(screen.getByRole('combobox', { name: /Platform provider/ }));
    await userEvent.click(await screen.findByRole('option', { name: 'platform-openai' }));
    await userEvent.click(screen.getByTestId('platform-model-save'));

    await waitFor(() => {
      expect(bodies).toHaveLength(1);
    });
    const sent = bodies[0] as { data: Record<string, unknown> };
    expect(sent.data.ai_credentials).toEqual({ elitea_title: 'platform-openai' });
    expect(sent.data.name).toBe('gpt-4o-mini');
  });

  it('sends the tier flags with a chat model, and sends none with a kind that has no such field', async () => {
    useModels([]);
    const bodies: unknown[] = [];
    server.use(
      http.post('*/admin/gateway/platform_models', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ id: 12 }, { status: 201 });
      }),
    );
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByTestId('platform-models-add'));
    await userEvent.type(await screen.findByTestId('platform-model-name'), 'my-model');
    await userEvent.type(screen.getByTestId('platform-model-wire-name'), 'gpt-4o-mini');
    await userEvent.click(screen.getByRole('combobox', { name: /Platform provider/ }));
    await userEvent.click(await screen.findByRole('option', { name: 'platform-openai' }));
    await userEvent.click(screen.getByTestId('platform-model-high-tier'));
    await userEvent.click(screen.getByTestId('platform-model-save'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    const chat = bodies[0] as { data: Record<string, unknown> };
    expect(chat.data.high_tier).toBe(true);
    expect(chat.data.low_tier).toBe(false);

    // The other four kinds decode their `data` with unknown fields REFUSED, so
    // a tier flag they do not declare would turn a valid model into an invalid
    // binding rather than a model with an ignored extra.
    await userEvent.click(screen.getByTestId('platform-models-add'));
    await userEvent.type(await screen.findByTestId('platform-model-name'), 'my-embedding');
    await userEvent.type(screen.getByTestId('platform-model-wire-name'), 'text-embedding-3');
    await userEvent.click(screen.getByRole('combobox', { name: 'Kind' }));
    await userEvent.click(await screen.findByRole('option', { name: 'Embedding' }));
    expect(screen.queryByTestId('platform-model-high-tier')).toBeNull();
    await userEvent.click(screen.getByRole('combobox', { name: /Platform provider/ }));
    await userEvent.click(await screen.findByRole('option', { name: 'platform-openai' }));
    await userEvent.click(screen.getByTestId('platform-model-save'));

    await waitFor(() => expect(bodies).toHaveLength(2));
    const embedding = bodies[1] as { data: Record<string, unknown> };
    expect(Object.hasOwn(embedding.data, 'high_tier')).toBe(false);
    expect(Object.hasOwn(embedding.data, 'low_tier')).toBe(false);
  });

  /*
   * The edit form reads the stored flags back. The update replaces `data`
   * whole, so a form that opened with both boxes clear would clear the tier of
   * every model it saved — the same shape as the credential link.
   */
  it('opens an edit on the tier the stored model already has', async () => {
    useModels([{ ...GPT4O, high_tier: true }]);
    const bodies: unknown[] = [];
    server.use(
      http.put('*/admin/gateway/platform_models/:id', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ id: 11 });
      }),
    );
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }));
    // Addressed by ROLE for the checked state: the test id sits on MUI's own
    // root span, and only the input underneath carries `checked`.
    expect(screen.getByRole('checkbox', { name: /high-tier/ })).toBeChecked();
    await userEvent.click(screen.getByTestId('platform-model-save'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    const sent = bodies[0] as { data: Record<string, unknown> };
    expect(sent.data.high_tier).toBe(true);
    expect(sent.data.ai_credentials).toEqual({ elitea_title: 'platform-openai' });
  });

  /*
   * DEFECT this pins. The dialog REBUILT `data` from the fields it shows, and
   * the update replaces the column whole — so every field it does not show was
   * erased by any save at all. An `llm_model` declares nine: a rename reset the
   * model's `context_window`, its `max_output_tokens` and its three capability
   * flags to the registry defaults, answered 200, and said nothing.
   *
   * The listing now carries the stored object and the submit merges over it, so
   * a field this dialog has never heard of survives an edit it was not part of.
   * `a_field_added_later` stands for the next one the registry adds.
   */
  it('keeps the stored fields the form does not show when only the label changes', async () => {
    useModels([
      {
        ...GPT4O,
        data: {
          name: 'gpt-4o',
          ai_credentials: { elitea_title: 'platform-openai' },
          context_window: 400000,
          max_output_tokens: 128000,
          supports_vision: true,
          supports_reasoning: true,
          openai_compatible: true,
          a_field_added_later: 'kept',
        },
      },
    ]);
    const bodies: unknown[] = [];
    server.use(
      http.put('*/admin/gateway/platform_models/:id', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ id: 11 });
      }),
    );
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }));
    // The stored values are what the form opens on — a form that opened on the
    // registry defaults would write those defaults over the row.
    expect(await screen.findByTestId('platform-model-context-window')).toHaveValue(400000);
    expect(screen.getByRole('checkbox', { name: /reasoning/i })).toBeChecked();

    await userEvent.clear(screen.getByTestId('platform-model-name'));
    await userEvent.type(screen.getByTestId('platform-model-name'), 'gpt-4o-renamed');
    await userEvent.click(screen.getByTestId('platform-model-save'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    const sent = bodies[0] as { elitea_title: string; data: Record<string, unknown> };
    expect(sent.elitea_title).toBe('gpt-4o-renamed');
    expect(sent.data.context_window).toBe(400000);
    expect(sent.data.supports_vision).toBe(true);
    expect(sent.data.supports_reasoning).toBe(true);
    expect(sent.data.openai_compatible).toBe(true);
    // Neither the form nor its types know this field. It survives because the
    // stored object is the merge BASE rather than a set of fields to re-read.
    expect(sent.data.max_output_tokens).toBe(128000);
    expect(sent.data.a_field_added_later).toBe('kept');
  });

  /* The capabilities are editable, and a change to one is what the save sends. */
  it('sends a capability the operator turned off', async () => {
    useModels([
      {
        ...GPT4O,
        data: {
          name: 'gpt-4o',
          ai_credentials: { elitea_title: 'platform-openai' },
          supports_vision: true,
        },
      },
    ]);
    const bodies: unknown[] = [];
    server.use(
      http.put('*/admin/gateway/platform_models/:id', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ id: 11 });
      }),
    );
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }));
    await userEvent.click(await screen.findByTestId('platform-model-supports-vision'));
    await userEvent.click(screen.getByTestId('platform-model-save'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect((bodies[0] as { data: Record<string, unknown> }).data.supports_vision).toBe(false);
  });

  it("renders the server's own refusal of a bad credential link", async () => {
    useModels([]);
    server.use(
      http.post('*/admin/gateway/platform_models', () =>
        HttpResponse.json(
          { error: 'no platform provider is named ghost. Published providers: platform-openai' },
          { status: 400 },
        ),
      ),
    );
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByTestId('platform-models-add'));
    await userEvent.type(await screen.findByTestId('platform-model-name'), 'm');
    await userEvent.type(screen.getByTestId('platform-model-wire-name'), 'x');
    // A provider IS chosen here. The refusal under test is the server's — the
    // named credential is gone from the platform between the listing and the
    // save — and the client-side rule can only say that nothing was chosen.
    await userEvent.click(screen.getByRole('combobox', { name: /Platform provider/ }));
    await userEvent.click(await screen.findByRole('option', { name: 'platform-openai' }));
    await userEvent.click(screen.getByTestId('platform-model-save'));

    expect(await screen.findByTestId('platform-model-dialog-error')).toHaveTextContent(
      'no platform provider is named ghost',
    );
  });

  /*
   * DEFECT this pins. The dialog opened on `modelTypes[0]`, and `model_types`
   * arrives in the SERVER's order — `asr_model` first, as `MODEL_TYPES` above
   * reproduces. So "Add a platform model" opened on "Speech to text". An
   * operator adding a chat model had to notice the wrong Kind and change it;
   * one who did not published a model the gateway dispatches to the ASR
   * section, and the model then answered nothing a chat caller asked for.
   */
  it('opens a new platform model on Chat / completion, whatever order the server lists the kinds in', async () => {
    useModels([]);
    const bodies: unknown[] = [];
    server.use(
      http.post('*/admin/gateway/platform_models', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ id: 12 }, { status: 201 });
      }),
    );
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByTestId('platform-models-add'));

    expect(await screen.findByRole('combobox', { name: 'Kind' })).toHaveTextContent('Chat / completion');

    // Asserted on the wire too: the visible label and the value sent are two
    // different things, and it is the value the gateway routes on.
    await userEvent.type(screen.getByTestId('platform-model-name'), 'my-model');
    await userEvent.type(screen.getByTestId('platform-model-wire-name'), 'gpt-4o-mini');
    await userEvent.click(screen.getByRole('combobox', { name: /Platform provider/ }));
    await userEvent.click(await screen.findByRole('option', { name: 'platform-openai' }));
    await userEvent.click(screen.getByTestId('platform-model-save'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    expect((bodies[0] as { type: string }).type).toBe('llm_model');
  });

  /* The default is a preference, not an override: a deployment that does not
     dispatch chat models must still get a Kind it can actually publish. */
  it('falls back to the first kind the deployment does dispatch', async () => {
    useModels([], { model_types: ['asr_model', 'tts_model'] });
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByTestId('platform-models-add'));

    expect(await screen.findByRole('combobox', { name: 'Kind' })).toHaveTextContent('Speech to text');
  });
});


describe('PlatformModelsPanel — deletion', () => {
  // Withdrawing a model from every project at once is not something a single
  // click on a table row should do, and the providers table beside this one
  // already confirms.
  it('confirms before withdrawing a model from every project', async () => {
    useModels([GPT4O]);
    const deletes: string[] = [];
    server.use(
      http.delete('*/admin/gateway/platform_models/:id', ({ params }) => {
        deletes.push(String(params.id));
        return new HttpResponse(null, { status: 204 });
      }),
    );
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByRole('button', { name: 'Delete' }));

    const confirm = await screen.findByTestId('platform-models-confirm-delete');
    expect(confirm).toHaveTextContent('every project at once');
    // Nothing has been deleted yet.
    expect(deletes).toEqual([]);

    await userEvent.click(screen.getByTestId('platform-models-confirm-delete-button'));
    await waitFor(() => {
      expect(deletes).toEqual(['11']);
    });
  });

  it('cancels without deleting', async () => {
    useModels([GPT4O]);
    const deletes: string[] = [];
    server.use(
      http.delete('*/admin/gateway/platform_models/:id', ({ params }) => {
        deletes.push(String(params.id));
        return new HttpResponse(null, { status: 204 });
      }),
    );
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByRole('button', { name: 'Delete' }));
    await userEvent.click(await screen.findByRole('button', { name: 'Cancel' }));

    await waitFor(() => {
      expect(screen.queryByTestId('platform-models-confirm-delete')).toBeNull();
    });
    expect(deletes).toEqual([]);
  });
});


/**
 * The GRANT: which projects a platform model is offered to.
 *
 * `shared = true` still means "this is a platform row"; the grant narrows it to
 * every project, to none, or to a chosen set. The panel is where an operator
 * both sets it and reads it back, so both directions are pinned here — and the
 * WIRE is asserted in each case, because the words on the screen and the value
 * the server stores are two different things.
 */
describe('PlatformModelsPanel — who the model is available to', () => {
  it('opens a new model on every project, and says so on the wire', async () => {
    useModels([]);
    const bodies: unknown[] = [];
    captureCreates(bodies);
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByTestId('platform-models-add'));
    // A NEW model is offered to everyone, which is what publishing one meant
    // before the grant existed. Opening on "no project" would make the ordinary
    // case the one that needs a second decision.
    expect(await screen.findByRole('combobox', { name: /Available to/ })).toHaveTextContent(
      'All projects',
    );
    await fillNewModel();
    await userEvent.click(screen.getByTestId('platform-model-save'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    const sent = (bodies[0] as { data: Record<string, unknown> }).data;
    expect(sent['share_scope']).toBe('all');
    expect(sent['shared_with']).toEqual([]);
  });

  it('sends the none scope with no projects', async () => {
    useModels([]);
    const bodies: unknown[] = [];
    captureCreates(bodies);
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByTestId('platform-models-add'));
    await fillNewModel();
    await userEvent.click(await screen.findByRole('combobox', { name: /Available to/ }));
    await userEvent.click(await screen.findByRole('option', { name: 'No project' }));
    await userEvent.click(screen.getByTestId('platform-model-save'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    const sent = (bodies[0] as { data: Record<string, unknown> }).data;
    expect(sent['share_scope']).toBe('none');
    expect(sent['shared_with']).toEqual([]);
  });

  it('sends the projects an operator picked, and will not save until one is picked', async () => {
    useModels([]);
    useProjects([
      { id: 90500, name: 'autotest-alpha' },
      { id: 42, name: 'autotest-beta' },
    ]);
    const bodies: unknown[] = [];
    captureCreates(bodies);
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByTestId('platform-models-add'));
    await fillNewModel();
    await userEvent.click(await screen.findByRole('combobox', { name: /Available to/ }));
    await userEvent.click(await screen.findByRole('option', { name: 'Selected projects' }));

    // "Selected projects" with nothing selected grants the model to nobody,
    // which is the OTHER choice on the same control. Disabled, not refused on
    // submit: the operator is told while they can still pick a project.
    expect(await screen.findByTestId('platform-model-shared-with')).toBeVisible();
    expect(screen.getByTestId('platform-model-save')).toBeDisabled();
    expect(bodies).toEqual([]);

    await userEvent.click(screen.getByRole('combobox', { name: /Projects/ }));
    await userEvent.click(await screen.findByRole('option', { name: 'autotest-alpha' }));
    await userEvent.click(screen.getByTestId('platform-model-save'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    const sent = (bodies[0] as { data: Record<string, unknown> }).data;
    expect(sent['share_scope']).toBe('projects');
    expect(sent['shared_with']).toEqual([90500]);
  });

  it('opens an edit on the grant the stored model already has, and keeps it', async () => {
    useModels([{ ...GPT4O, share_scope: 'projects', shared_with: [90500] }]);
    useProjects([{ id: 90500, name: 'autotest-alpha' }]);
    const bodies: unknown[] = [];
    server.use(
      http.put('*/admin/gateway/platform_models/:id', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ id: 11 });
      }),
    );
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }));
    // The `data` column is replaced whole, so a form that opened on the default
    // would write "every project" over a model somebody had restricted.
    expect(await screen.findByRole('combobox', { name: /Available to/ })).toHaveTextContent(
      'Selected projects',
    );
    await userEvent.click(screen.getByTestId('platform-model-save'));

    await waitFor(() => expect(bodies).toHaveLength(1));
    const sent = (bodies[0] as { data: Record<string, unknown> }).data;
    expect(sent['share_scope']).toBe('projects');
    expect(sent['shared_with']).toEqual([90500]);
  });

  it('clears the project list when the grant is withdrawn', async () => {
    useModels([{ ...GPT4O, share_scope: 'projects', shared_with: [90500] }]);
    useProjects([{ id: 90500, name: 'autotest-alpha' }]);
    const bodies: unknown[] = [];
    server.use(
      http.put('*/admin/gateway/platform_models/:id', async ({ request }) => {
        bodies.push(await request.json());
        return HttpResponse.json({ id: 11 });
      }),
    );
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByRole('button', { name: 'Edit' }));
    await userEvent.click(await screen.findByRole('combobox', { name: /Available to/ }));
    await userEvent.click(await screen.findByRole('option', { name: 'All projects' }));
    await userEvent.click(screen.getByTestId('platform-model-save'));

    // The list would otherwise ride along in the merge base and the next switch
    // back to "selected projects" would restore a grant nobody re-chose.
    await waitFor(() => expect(bodies).toHaveLength(1));
    const sent = (bodies[0] as { data: Record<string, unknown> }).data;
    expect(sent['share_scope']).toBe('all');
    expect(sent['shared_with']).toEqual([]);
  });

  it('reports each grant on the row', async () => {
    useModels([
      { ...GPT4O, id: 1, elitea_title: 'to-all', share_scope: 'all' },
      { ...GPT4O, id: 2, elitea_title: 'to-none', share_scope: 'none' },
      { ...GPT4O, id: 3, elitea_title: 'to-two', share_scope: 'projects', shared_with: [1, 2] },
      // A server that predates the field sends neither. Every such model is
      // offered to every project, so the chip must say so — a blank there reads
      // as "granted to nobody" for the whole deployment.
      { ...GPT4O, id: 4, elitea_title: 'legacy' },
    ]);
    renderAdminRoute(<PlatformModelsPanel />);

    await screen.findByTestId('platform-models-table');
    const chips = screen.getAllByTestId('platform-model-scope');
    expect(chips.map((chip) => chip.textContent)).toEqual([
      'All projects',
      'No project',
      '2 projects',
      'All projects',
    ]);
  });
});
