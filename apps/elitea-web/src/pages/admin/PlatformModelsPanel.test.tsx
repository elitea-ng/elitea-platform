/**
 * The platform models panel.
 *
 * The assertions are the two states the GATEWAY will not fail on, plus the one
 * shape the server owns:
 *
 *  1. **A model naming a provider the platform does not publish.** The gateway
 *     still advertises it and guesses the provider from a prefix in the model
 *     name, saying so only in a log line. This screen is the only place it is
 *     visible.
 *  2. **`status_ok = false`.** Stored, listed, never dispatched.
 *  3. **The credential select offers the server's list**, and "None" is a real
 *     option rather than a placeholder — a model with no link resolves by
 *     prefix, which is a supported configuration.
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
    expect(warning).toHaveTextContent('guesses the provider');
  });

  it('does not flag a model that names no provider at all', async () => {
    useModels([{ ...GPT4O, credential_name: '', credential_resolves: true }]);
    renderAdminRoute(<PlatformModelsPanel />);

    await screen.findByTestId('platform-models-table');
    // Resolving by prefix is a supported configuration the standalone seed
    // relies on; flagging it would be a permanent false alarm.
    expect(screen.queryByTestId('platform-models-unresolved')).toBeNull();
    expect(screen.getByTestId('platform-models-table')).toHaveTextContent('inferred from the name');
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

  it('offers the platform providers and a real "none" option', async () => {
    useModels([]);
    renderAdminRoute(<PlatformModelsPanel />);

    await userEvent.click(await screen.findByTestId('platform-models-add'));
    // Addressed by its accessible NAME rather than by index: an index would
    // silently follow whichever select happened to be second if a field were
    // added above it.
    await userEvent.click(await screen.findByRole('combobox', { name: 'Platform provider' }));

    expect(await screen.findByRole('option', { name: 'platform-openai' })).toBeVisible();
    expect(screen.getByRole('option', { name: /None/ })).toBeVisible();
  });

  it('omits the credential link entirely when none is chosen', async () => {
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
    await userEvent.click(screen.getByTestId('platform-model-save'));

    await waitFor(() => {
      expect(bodies).toHaveLength(1);
    });
    const sent = bodies[0] as { data: Record<string, unknown> };
    // An empty link object would make a row that SAYS it has a credential and
    // does not; the gateway reads such a link as naming nothing, so writing one
    // only misleads the next reader.
    expect(Object.hasOwn(sent.data, 'ai_credentials')).toBe(false);
    expect(sent.data.name).toBe('gpt-4o-mini');
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
