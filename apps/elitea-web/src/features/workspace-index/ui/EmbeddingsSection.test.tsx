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

function setup(options: Omit<FakeIndexOptions, 'statuses'> = {}): FakeIndexIpc {
  const ipc = createFakeIndexIpc({ statuses: { w1: READY }, ...options });
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
});
