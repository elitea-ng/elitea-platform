/**
 * The editor's live test chat — the slot that used to render a "not
 * available yet" notice.
 *
 * What is pinned here is the part a green `EditPipeline.test.tsx` still could
 * not see: WHEN the conversation is created, and WHAT it is created with. A
 * pipeline conversation that attaches cleanly and then answers 422 on every
 * send is the classic shape of this defect (`ChatWithPipelineButton.tsx`'s
 * own header enumerates all three ways to produce one), and none of it is
 * visible from "a composer rendered".
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import type { ReactElement } from 'react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import { renderPipelinesRoute } from '../__tests__/testRouter';
import { resetPipelineTestConversationsForTests } from '../lib/usePipelineTestConversation';
import { PipelineTestChat } from './PipelineTestChat';

const globals = globalThis as unknown as Record<string, unknown>;

const IDENTITY = {
  projectId: '9',
  applicationId: '42',
  pipelineName: 'My Pipeline',
  versionId: '7',
  agentType: 'pipeline',
};

const USER = { id: '6', name: 'E2E Chat Driver', avatar: '' };

interface ParticipantRow {
  readonly entity_name?: string;
  readonly entity_meta?: { readonly id?: unknown };
  readonly entity_settings?: { readonly version_id?: unknown; readonly agent_type?: unknown };
}

/** Every request the bootstrap makes, plus the participant rows it posted. */
function installConversationRoutes(): { readonly posted: ParticipantRow[][]; readonly creates: number } {
  const posted: ParticipantRow[][] = [];
  const counter = { creates: 0 };
  server.use(
    http.post('*/elitea_core/conversations/prompt_lib/:projectId', async ({request}) => {
      const body=await request.json() as {readonly source:string;readonly meta:{readonly is_hidden:boolean};readonly participants:ParticipantRow[]};
      counter.creates += 1;posted.push(body.participants);
      expect(body.source).toBe('editor_test');expect(body.meta.is_hidden).toBe(true);
      return HttpResponse.json({id:501,uuid:'00000000-0000-4000-8000-000000000abc',name:'My Pipeline',source:'editor_test',is_private:true,
        meta:{is_hidden:true,editor_test:{revision:1,actor_id:'6',project_id:'9',application_id:'42',application_version_id:'7'}},
        participants:[{id:'900',entity_name:'user',entity_meta:{id:6}},{id:'901',entity_name:'application',entity_meta:{id:'42',project_id:'9'},entity_settings:{version_id:'7',agent_type:'pipeline'}}]});
    }),
    http.post('*/elitea_core/participants/prompt_lib/:projectId/:conversationId', async ({ request }) => {
      await request.json();
      throw new Error('Test participants must be atomic');
    }),
    http.get('*/elitea_core/conversation/prompt_lib/:projectId/:conversationId', () =>
      HttpResponse.json({
        id: 501,
        uuid: '00000000-0000-4000-8000-000000000abc',
        name: 'My Pipeline',
        participants: [
          { id: '900', entity_name: 'user', entity_meta: { id: 6 } },
          { id: '901', entity_name: 'application', entity_meta: { id: '42' }, entity_settings: { version_id: '7', agent_type: 'pipeline' } },
        ],
      }),
    ),
    http.get('*/configurations/models/:projectId', () =>
      HttpResponse.json({ items: [{ name: 'gpt-4o', display_name: 'GPT-4o', project_id: '9', default: true }], default_model_name: 'gpt-4o' }),
    ),
    http.get('*/configurations/tts_voices/*', () => HttpResponse.json({ items: [] })),
  );
  return { posted, get creates() { return counter.creates; } };
}

/** The pane on its own, inside this unit's own router/socket/theme fixture — `ChatBox` reads the router context for the selected project. */
function renderPane(node: ReactElement): void {
  renderPipelinesRoute(node, '/pipelines/all/42', { projectId: '9' });
}

beforeEach(() => {
  globals['elitea_ui_config'] = { vite_server_url: '/api/v2', vite_base_uri: '/', vite_public_project_id: 'public-1' };
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: '/api/v2' });
  // The pane reuses a conversation across MOUNTS via a module-scope map, which
  // outlives a `render`. Without this reset the create-count assertions below
  // would depend on which test ran first.
  resetPipelineTestConversationsForTests();
});

afterEach(() => {
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  resetGeneratedClient();
});

describe('PipelineTestChat', () => {
  it('displays the pipeline model before creating a test conversation', async () => {
    const routes = installConversationRoutes();
    server.use(http.get('*/configurations/models/:projectId', () => HttpResponse.json({
      items: [
        { id: 'default', name: 'gpt-4o', display_name: 'GPT-4o', project_id: '9', default: true },
        { id: 'pipeline', name: 'pipeline-model', display_name: 'Saved pipeline model', project_id: '1' },
      ], default_model_name: 'gpt-4o',
    })));
    renderPane(<PipelineTestChat
      settings={{ llmSettings: { model_name: 'pipeline-model', model_project_id: '1' } }}
      disableChat={false} slotRef={undefined} identity={IDENTITY} user={USER}
    />);
    expect(await screen.findByRole('button', { name: /Saved pipeline model/ })).toBeInTheDocument();
    expect(routes.creates).toBe(0);
  });

  it('creates no conversation until the pane is actually used', async () => {
    const routes = installConversationRoutes();
    renderPane(
      <PipelineTestChat
        settings={{}}
        disableChat={false}
        slotRef={undefined}
        identity={IDENTITY}
        user={USER}
      />,
    );

    // The real composer is mounted...
    expect(await screen.findByTestId('chat-message-input')).toBeInTheDocument();
    // ...and opening the editor to READ a graph has minted nothing. An
    // eager bootstrap would leave one private conversation per page view in
    // the user's own sidebar.
    await waitFor(() => expect(routes.creates).toBe(0));
  });

  it('creates the saved application atomically and receives the server-owned user on first use', async () => {
    const routes = installConversationRoutes();
    const user = userEvent.setup();
    renderPane(
      <PipelineTestChat
        settings={{}}
        disableChat={false}
        slotRef={undefined}
        identity={IDENTITY}
        user={USER}
      />,
    );

    await user.click(await screen.findByTestId('chat-message-input'));

    await waitFor(() => expect(routes.posted).toHaveLength(1));
    const rows = routes.posted[0] ?? [];
    expect(rows).toHaveLength(1);
    expect(rows.some((row)=>row.entity_name==='user')).toBe(false);
    const pipelineRow = rows.find((row) => row.entity_name === 'application');
    expect(pipelineRow, 'a pipeline participant must still be named `application`').toBeDefined();
    expect(pipelineRow?.entity_settings?.version_id).toBe('7');
    expect(pipelineRow?.entity_settings?.agent_type).toBe('pipeline');

    // Exactly one conversation, however many events that one click produced
    // (pointer-down AND focus both call `ensure`).
    await waitFor(() => expect(routes.creates).toBe(1));
  });

  it('Clear releases the local test context without creating or deleting durable records', async () => {
    const routes = installConversationRoutes();
    const user = userEvent.setup();
    let deleted = 0;
    server.use(http.delete('*/elitea_core/*', () => {
      deleted += 1;
      return new HttpResponse(null, { status: 204 });
    }));
    renderPane(<PipelineTestChat settings={{}} disableChat={false} slotRef={undefined} identity={IDENTITY} user={USER} />);

    expect(await screen.findByTestId('editor-test-clear')).toBeDisabled();
    await user.click(screen.getByTestId('chat-message-input'));
    await waitFor(() => expect(screen.getByTestId('editor-test-clear')).toBeEnabled());
    await user.click(screen.getByTestId('editor-test-clear'));
    await waitFor(() => expect(screen.getByTestId('editor-test-clear')).toBeDisabled());
    expect(routes.creates).toBe(1);
    expect(deleted).toBe(0);
  });

  it('creates nothing while the version has not resolved — a participant with no version_id 422s every turn', async () => {
    const routes = installConversationRoutes();
    const user = userEvent.setup();
    renderPane(
      <PipelineTestChat
        settings={{}}
        disableChat={false}
        slotRef={undefined}
        identity={{ ...IDENTITY, versionId: undefined }}
        user={USER}
      />,
    );

    await user.click(await screen.findByTestId('chat-message-input'));
    await waitFor(() => expect(routes.creates).toBe(0));
  });

  it("renders the version's own conversation starters, which are plain strings", async () => {
    installConversationRoutes();
    renderPane(
      <PipelineTestChat
        settings={{ conversationStarters: ['Explain this pipeline'] }}
        disableChat={false}
        slotRef={undefined}
        identity={IDENTITY}
        user={USER}
      />,
    );

    // The starters a pipeline version stores are strings, and
    // `ChatConversationStarters` stringifies whatever it renders. The pane
    // used to keep only `{id, text}` rows — a shape nothing produces and
    // nothing reads — so every real starter was filtered out before it could
    // reach the grid, and the empty chat showed no suggestions at all.
    expect(await screen.findByText('Explain this pipeline')).toBeInTheDocument();
  });

  it('shows why the chat is closed instead of a composer that could not send', async () => {
    installConversationRoutes();
    renderPane(
      <PipelineTestChat
        settings={{}}
        disableChat
        slotRef={undefined}
        identity={IDENTITY}
        user={USER}
      />,
    );

    expect(await screen.findByTestId('edit-pipeline-test-chat-disabled')).toBeInTheDocument();
    expect(screen.queryByTestId('chat-message-input')).not.toBeInTheDocument();
  });

  it('bridges the slot handle onto ChatBox, which is what makes the canvas Stop button work', async () => {
    installConversationRoutes();
    const slotRef: { current: { stopAll: () => void; onClear: () => void } | null } = { current: null };
    renderPane(
      <PipelineTestChat
        settings={{}}
        disableChat={false}
        slotRef={slotRef}
        identity={IDENTITY}
        user={USER}
      />,
    );

    await screen.findByTestId('chat-message-input');
    // `ChatPanel.stopRun` (wired to `EditorPanel`'s stop) calls exactly these
    // two. They must not throw: the whole point of the bridge is that a
    // narrower `ChatBoxSlotHandle` ref reaches the wider `ChatBoxHandle`.
    expect(slotRef.current).not.toBeNull();
    expect(() => slotRef.current?.stopAll()).not.toThrow();
    expect(() => slotRef.current?.onClear()).not.toThrow();
  });
});
