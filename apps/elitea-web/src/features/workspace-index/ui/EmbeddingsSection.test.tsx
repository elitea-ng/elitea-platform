/**
 * Semantic search in the index dialog, over the in-memory host fake: off by
 * default with nothing downloaded, a model downloads only after the person
 * agrees to its size and is applied once ready, progress and the power hold
 * show, and the deployment option appears only when the policy allows it.
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { type IndexStatus } from '@/shared/desktop/indexIpc';
import { createFakeIndexIpc, type FakeIndexIpc, type FakeIndexOptions } from '@/shared/desktop/indexIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { IndexIpcProvider } from '../model/ipcContext';
import { WorkspaceIndexControl } from './WorkspaceIndexControl';

const READY: Partial<IndexStatus> = {
  state: 'ready',
  files: 120,
  entities: 12_400,
  relations: 3_000,
  last_run: '2026-10-10T08:00:00Z',
};

function setup(options: Omit<FakeIndexOptions, 'statuses'> = {}, status: Partial<IndexStatus> = READY): FakeIndexIpc {
  const ipc = createFakeIndexIpc({ statuses: { w1: status }, ...options });
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  renderWithTheme(
    <QueryClientProvider client={client}>
      <IndexIpcProvider ipc={ipc}>
        <WorkspaceIndexControl workspaceId="w1" name="app" />
      </IndexIpcProvider>
    </QueryClientProvider>,
  );
  return ipc;
}

async function section(user: ReturnType<typeof userEvent.setup>): Promise<HTMLElement> {
  await user.click(await screen.findByTestId('index-status-chip'));
  const dialog = await screen.findByRole('dialog', {
    name: 'Code index · app',
  });
  const found = await within(dialog).findByTestId('index-embeddings');
  await within(found).findByRole('radiogroup');
  return found;
}

describe('EmbeddingsSection', () => {
  it('is off by default, lists both models with sizes and memory, and offers no deployment option', async () => {
    const user = userEvent.setup();
    setup();
    const embeddings = await section(user);
    expect(within(embeddings).getByRole('radio', { name: 'Off' })).toBeChecked();
    expect(embeddings).toHaveTextContent('Qwen3 Embedding 0.6B');
    expect(embeddings).toHaveTextContent('1.2 GB download · about 2.6 GB of memory while embedding');
    expect(embeddings).toHaveTextContent('470 MB download · about 1.4 GB of memory while embedding');
    expect(
      within(embeddings).queryByRole('radio', {
        name: 'Through your deployment',
      }),
    ).toBeNull();
  });

  it('asks before downloading, downloads on consent, and applies the model once it is ready', async () => {
    const user = userEvent.setup();
    const ipc = setup();
    const embeddings = await section(user);
    await user.click(within(embeddings).getByRole('radio', { name: /multilingual-e5-small/ }));
    const consent = await within(embeddings).findByTestId('index-model-consent');
    expect(consent).toHaveTextContent('Download multilingual-e5-small (470 MB)');
    // Choosing is not consent: nothing is downloaded or set yet.
    expect(ipc.embeddingCalls.downloads).toEqual([]);
    expect(ipc.embeddingCalls.set).toEqual([]);

    await user.click(within(consent).getByRole('button', { name: 'Download' }));
    expect(ipc.embeddingCalls.downloads).toEqual(['multilingual-e5-small']);
    act(() =>
      ipc.emitModel({
        kind: 'model',
        model: 'multilingual-e5-small',
        phase: 'downloading',
        done: 235_000_000,
        total: 470_000_000,
        error: null,
      }),
    );
    expect(await within(embeddings).findByTestId('index-model-download')).toHaveTextContent('235 MB of 470 MB');

    act(() =>
      ipc.emitModel({
        kind: 'model',
        model: 'multilingual-e5-small',
        phase: 'ready',
        done: 470_000_000,
        total: 470_000_000,
        error: null,
      }),
    );
    await waitFor(() => expect(ipc.embeddingCalls.set).toEqual([{ workspaceId: 'w1', choice: 'local:multilingual-e5-small' }]));
    await waitFor(() =>
      expect(
        within(embeddings).getByRole('radio', {
          name: /multilingual-e5-small/,
        }),
      ).toBeChecked(),
    );
    expect(within(embeddings).queryByTestId('index-model-consent')).toBeNull();
  });

  it('uses a downloaded model at once, shows the progress and embeds now when held on battery', async () => {
    const user = userEvent.setup();
    const ipc = setup();
    ipc.setModel('multilingual-e5-small', { state: 'ready' });
    const embeddings = await section(user);
    await user.click(within(embeddings).getByRole('radio', { name: /multilingual-e5-small/ }));
    await waitFor(() => expect(ipc.embeddingCalls.set).toEqual([{ workspaceId: 'w1', choice: 'local:multilingual-e5-small' }]));
    expect(ipc.embeddingCalls.downloads).toEqual([]);

    const status = {
      choice: 'local:multilingual-e5-small:384',
      done: 10,
      total: 40,
      error: null,
      forced: false,
      eta_seconds: null,
      tokens: 0,
      cap: null,
    };
    act(() =>
      ipc.emitEmbeddings({
        kind: 'embeddings',
        workspace_id: 'w1',
        status: { ...status, state: 'paused', held: 'on_battery' },
      }),
    );
    expect(await within(embeddings).findByTestId('index-embeddings-status')).toHaveTextContent('Paused on battery (10 of 40)');
    await user.click(within(embeddings).getByRole('button', { name: 'Embed now' }));
    expect(ipc.embeddingCalls.rebuilds).toEqual([{ workspaceId: 'w1', full: false }]);

    act(() =>
      ipc.emitEmbeddings({
        kind: 'embeddings',
        workspace_id: 'w1',
        status: { ...status, done: 40, state: 'done', held: null },
      }),
    );
    await waitFor(() => expect(within(embeddings).getByTestId('index-embeddings-status')).toHaveTextContent('40 chunks embedded'));
  });

  it('offers the deployment only when the policy allows it, with a warning, and sends the model name', async () => {
    const user = userEvent.setup();
    const ipc = setup({ embeddingsAllowed: { remote: true } });
    const embeddings = await section(user);
    await user.click(
      within(embeddings).getByRole('radio', {
        name: 'Through your deployment',
      }),
    );
    expect(embeddings).toHaveTextContent('code is sent to your deployment’s embeddings model');
    await user.type(within(embeddings).getByRole('textbox', { name: 'Embeddings model' }), 'text-embedding-3-small');
    await user.click(within(embeddings).getByRole('button', { name: 'Use' }));
    await waitFor(() => expect(ipc.embeddingCalls.set).toEqual([{ workspaceId: 'w1', choice: 'remote:text-embedding-3-small' }]));
  });

  it('disables the models when the policy refuses embedding on this computer', async () => {
    const user = userEvent.setup();
    setup({ embeddingsAllowed: { local: false } });
    const embeddings = await section(user);
    expect(within(embeddings).getByRole('radio', { name: /multilingual-e5-small/ })).toBeDisabled();
    expect(embeddings).toHaveTextContent('does not allow embedding models on this computer');
  });

  it('recommends the code model for a small index and the small one above 50k chunks', async () => {
    const user = userEvent.setup();
    setup({}, { ...READY, chunks: 4_000 });
    const embeddings = await section(user);
    expect(within(embeddings).getByRole('radio', { name: /Qwen3 Embedding 0\.6B · recommended/ })).toBeInTheDocument();
    expect(within(embeddings).getByRole('radio', { name: /multilingual-e5-small/ })).not.toHaveAccessibleName(/recommended/);
  });

  it('recommends multilingual-e5-small for a large index, and the person still picks', async () => {
    const user = userEvent.setup();
    setup({}, { ...READY, chunks: 256_000 });
    const embeddings = await section(user);
    expect(within(embeddings).getByRole('radio', { name: /multilingual-e5-small · recommended/ })).toBeInTheDocument();
    expect(within(embeddings).getByRole('radio', { name: 'Off' })).toBeChecked();
  });

  it('shows the time left while it embeds', async () => {
    const user = userEvent.setup();
    const ipc = setup({ embeddings: { w1: 'local:multilingual-e5-small' } });
    const embeddings = await section(user);
    act(() =>
      ipc.emitEmbeddings({
        kind: 'embeddings',
        workspace_id: 'w1',
        status: { choice: 'local:multilingual-e5-small', state: 'running', done: 1_000, total: 9_000, held: null, error: null, forced: false, eta_seconds: 7_500, tokens: 0, cap: null },
      }),
    );
    expect(await within(embeddings).findByTestId('index-embeddings-status')).toHaveTextContent('Embedding: 1000 of 9000 chunks, about 2 h 5 min left.');
  });

  it('imports a model from a picked folder (a cancelled pick changes nothing) and removes it', async () => {
    const user = userEvent.setup();
    const ipc = setup();
    const embeddings = await section(user);
    const files = within(embeddings).getByTestId('index-model-files');
    ipc.cancelNextPick();
    await user.click(within(files).getAllByRole('button', { name: 'Import model files…' })[1]!);
    expect(ipc.embeddingCalls.imported).toEqual([]);
    await user.click(within(files).getAllByRole('button', { name: 'Import model files…' })[1]!);
    await waitFor(() => expect(ipc.embeddingCalls.imported).toEqual(['multilingual-e5-small']));
    const remove = await within(files).findByRole('button', { name: 'Remove model' });
    expect(embeddings).toHaveTextContent('Downloaded · about 1.4 GB of memory while embedding');
    await user.click(remove);
    await waitFor(() => expect(ipc.embeddingCalls.removed).toEqual(['multilingual-e5-small']));
    await waitFor(() => expect(within(files).queryByRole('button', { name: 'Remove model' })).not.toBeInTheDocument());
    expect(ipc.embeddingCalls.downloads).toEqual([]);
  });

  it('names the organisation’s mirror in the download consent', async () => {
    const user = userEvent.setup();
    setup({ mirror: 'models.example.com' });
    const embeddings = await section(user);
    await user.click(within(embeddings).getByRole('radio', { name: /multilingual-e5-small/ }));
    expect(await within(embeddings).findByTestId('index-model-consent')).toHaveTextContent('from your organisation’s mirror (models.example.com)');
  });

  it('picks the deployment model from the project’s list when there is one', async () => {
    const user = userEvent.setup();
    const ipc = setup({
      embeddingsAllowed: { remote: true },
      remoteModels: [
        { name: 'text-embedding-3-small', display_name: 'OpenAI small', price_per_1m: null, currency: null },
        { name: 'nomic-embed', display_name: null, price_per_1m: null, currency: null },
      ],
    });
    const embeddings = await section(user);
    await user.click(within(embeddings).getByRole('radio', { name: 'Through your deployment' }));
    expect(within(embeddings).queryByRole('textbox', { name: 'Embeddings model' })).not.toBeInTheDocument();
    await user.click(await within(embeddings).findByRole('combobox', { name: 'Embeddings model' }));
    await user.click(await screen.findByRole('option', { name: 'nomic-embed' }));
    await user.click(within(embeddings).getByRole('button', { name: 'Use' }));
    await waitFor(() => expect(ipc.embeddingCalls.set).toEqual([{ workspaceId: 'w1', choice: 'remote:nomic-embed' }]));
  });

  it('falls back to a typed name when the project’s models cannot be listed', async () => {
    const user = userEvent.setup();
    const ipc = setup({ embeddingsAllowed: { remote: true } });
    ipc.failNext('remoteModels', 'models_unavailable', 'unreachable');
    const embeddings = await section(user);
    await user.click(within(embeddings).getByRole('radio', { name: 'Through your deployment' }));
    expect(within(embeddings).getByRole('textbox', { name: 'Embeddings model' })).toBeInTheDocument();
  });
  it('shows the estimate for the deployment model before it is used, with the budget and the cost in their currencies', async () => {
    const user = userEvent.setup();
    const ipc = setup({
      embeddingsAllowed: { remote: true },
      remoteModels: [{ name: 'm', display_name: null, price_per_1m: 0.02, currency: 'USD' }],
      estimate: {
        tokens: 2_500_000,
        texts: 9_000,
        days_needed: 13,
        project_budget: { unlimited: false, remaining: 0.03, currency: 'USD', percent_used: 97.4, warning_active: true },
      },
    });
    const embeddings = await section(user);
    await user.click(within(embeddings).getByRole('radio', { name: 'Through your deployment' }));
    const estimate = await within(embeddings).findByTestId('index-embedding-estimate');
    // The runtime's locale writes USD as `$` or `US$`.
    expect(estimate).toHaveTextContent(/About 2,500,000 tokens to embed \(9,000 chunks\)\. This is an estimate\. Estimated cost: about (US)?\$0\.05\./);
    expect(estimate).toHaveTextContent(/Project budget left this month: (US)?\$0\.03 \(97% used\)\./);
    expect(estimate).toHaveTextContent('more than the project has left');
    expect(estimate).toHaveTextContent('A run stops at 2,000,000 tokens and a day at 200,000 (0 used today); this folder needs about 13 day(s)');
    expect(ipc.embeddingCalls.estimates.at(-1)).toEqual({ workspaceId: 'w1', choice: 'remote:m' });
  });

  it('shows amounts in the currency the platform names, and compares only one currency', async () => {
    const user = userEvent.setup();
    setup({
      embeddingsAllowed: { remote: true },
      remoteModels: [{ name: 'm', display_name: null, price_per_1m: 0.02, currency: 'EUR' }],
      estimate: { tokens: 2_500_000, project_budget: { unlimited: false, remaining: 0.03, currency: 'GBP', percent_used: 97.4, warning_active: true } },
    });
    const embeddings = await section(user);
    await user.click(within(embeddings).getByRole('radio', { name: 'Through your deployment' }));
    const estimate = await within(embeddings).findByTestId('index-embedding-estimate');
    expect(estimate).toHaveTextContent('Estimated cost: about €0.05.');
    expect(estimate).toHaveTextContent('Project budget left this month: £0.03 (97% used).');
    expect(estimate).not.toHaveTextContent('more than the project has left');
  });

  it('asks for the typed model’s estimate once the field rests, not per keystroke', async () => {
    const user = userEvent.setup();
    const ipc = setup({ embeddingsAllowed: { remote: true } });
    ipc.failNext('remoteModels', 'models_unavailable', 'unreachable');
    const embeddings = await section(user);
    await user.click(within(embeddings).getByRole('radio', { name: 'Through your deployment' }));
    await user.type(within(embeddings).getByRole('textbox', { name: 'Embeddings model' }), 'text-embedding');
    await within(embeddings).findByTestId('index-embedding-estimate');
    expect(ipc.embeddingCalls.estimates).toEqual([{ workspaceId: 'w1', choice: 'remote:text-embedding' }]);
  });

  it('says the cost is unknown when the catalogue has no price and keeps the percentage for a member who may not see amounts', async () => {
    const user = userEvent.setup();
    setup({
      embeddingsAllowed: { remote: true },
      estimate: { project_budget: { unlimited: false, remaining: null, currency: null, percent_used: 40.2, warning_active: false } },
    });
    const embeddings = await section(user);
    await user.click(within(embeddings).getByRole('radio', { name: 'Through your deployment' }));
    await user.type(within(embeddings).getByRole('textbox', { name: 'Embeddings model' }), 'm');
    const estimate = await within(embeddings).findByTestId('index-embedding-estimate');
    expect(estimate).not.toHaveTextContent('Estimated cost');
    expect(estimate).toHaveTextContent('40% of the project’s monthly budget is used.');
  });

  it('saves the caps the person sets, refusing nonsense', async () => {
    const user = userEvent.setup();
    const ipc = setup({ embeddingsAllowed: { remote: true }, embeddings: { w1: 'remote:8:m' } });
    const embeddings = await section(user);
    await within(embeddings).findByTestId('index-embedding-caps');
    // Query the fields afresh each time: a refetched estimate re-renders the
    // caps form, and a held element can be a detached node by the click.
    const save = (): HTMLElement => within(within(embeddings).getByTestId('index-embedding-caps')).getByRole('button', { name: 'Save limits' });
    const day = (): HTMLElement => within(within(embeddings).getByTestId('index-embedding-caps')).getByRole('textbox', { name: 'Tokens per day' });
    expect(save()).toBeDisabled();
    await user.clear(day());
    await user.type(day(), '0');
    expect(save()).toBeDisabled();
    await user.clear(day());
    await user.type(day(), '900000');
    await waitFor(() => expect(save()).toBeEnabled());
    await user.click(save());
    await waitFor(() => expect(ipc.embeddingCalls.caps).toEqual([{ workspaceId: 'w1', caps: { run: 2_000_000, day: 900_000 } }]));
    await waitFor(() => expect(within(embeddings).getByTestId('index-embedding-estimate')).toHaveTextContent('a day at 900,000'));
  });

  it('shows why a capped run stopped, and only a run cap offers embed now', async () => {
    const user = userEvent.setup();
    const ipc = setup({ embeddings: { w1: 'remote:8:m' }, embeddingsAllowed: { remote: true } });
    const embeddings = await section(user);
    const status = { choice: 'remote:m:8', done: 300, total: 900, held: null, error: null, forced: false, eta_seconds: null, tokens: 2_000_000 } as const;
    act(() => ipc.emitEmbeddings({ kind: 'embeddings', workspace_id: 'w1', status: { ...status, state: 'capped', cap: 'run' } }));
    const line = await within(embeddings).findByTestId('index-embeddings-status');
    expect(line).toHaveTextContent('Stopped at this run’s token limit (300 of 900 chunks)');
    await user.click(within(line).getByRole('button', { name: 'Embed now' }));
    expect(ipc.embeddingCalls.rebuilds).toEqual([{ workspaceId: 'w1', full: false }]);
    act(() => ipc.emitEmbeddings({ kind: 'embeddings', workspace_id: 'w1', status: { ...status, state: 'capped', cap: 'day' } }));
    await waitFor(() => expect(within(embeddings).getByTestId('index-embeddings-status')).toHaveTextContent('Stopped at today’s token limit'));
    expect(within(embeddings).queryByRole('button', { name: 'Embed now' })).toBeNull();
  });

  it('shows an exhausted budget as its own state and tries again only when asked', async () => {
    const user = userEvent.setup();
    const ipc = setup({ embeddings: { w1: 'remote:8:m' }, embeddingsAllowed: { remote: true } });
    const embeddings = await section(user);
    act(() =>
      ipc.emitEmbeddings({
        kind: 'embeddings',
        workspace_id: 'w1',
        status: {
          choice: 'remote:m:8',
          state: 'budget_exhausted',
          done: 300,
          total: 900,
          held: null,
          error: 'model_gateway.project_budget_exhausted (HTTP 402)',
          forced: false,
          eta_seconds: null,
          tokens: 40_000,
          cap: null,
        },
      }),
    );
    const line = await within(embeddings).findByTestId('index-embeddings-status');
    expect(line).toHaveTextContent('The project’s budget is used up (300 of 900 chunks)');
    expect(line).toHaveTextContent('project_budget_exhausted (HTTP 402)');
    expect(ipc.embeddingCalls.rebuilds).toEqual([]);
    await user.click(within(line).getByRole('button', { name: 'Try again' }));
    expect(ipc.embeddingCalls.rebuilds).toEqual([{ workspaceId: 'w1', full: false }]);
  });
});
