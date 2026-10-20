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
        status: { choice: 'local:multilingual-e5-small', state: 'running', done: 1_000, total: 9_000, held: null, error: null, forced: false, eta_seconds: 7_500 },
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
        { name: 'text-embedding-3-small', display_name: 'OpenAI small' },
        { name: 'nomic-embed', display_name: null },
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
});
