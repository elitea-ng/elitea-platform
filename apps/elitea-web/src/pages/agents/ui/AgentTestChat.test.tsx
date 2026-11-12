/**
 * The agent page's test chat (#1974, #1980).
 *
 * What is pinned is WHEN the conversation is created and WHAT it is created
 * with: an `editor_test` conversation (hidden, private, bound to this agent
 * version by Main) carrying the agent as its one participant, so the chat
 * belongs to the agent and never reaches the Chats sidebar list.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import type { ReactElement } from 'react';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetConfigForTests } from '@/shared/config/get-config';
import { server } from '@/test/setup';

import { renderAgentsRoute } from '../__tests__/testRouter';
import { AgentTestChat } from './AgentTestChat';

const globals = globalThis as unknown as Record<string, unknown>;

const IDENTITY = { projectId: '9', applicationId: '42', agentName: 'My Agent', versionId: '7', agentType: 'openai' };
const USER = { id: '6', name: 'E2E Chat Driver', avatar: '' };

interface ParticipantRow {
  readonly entity_name?: string;
  readonly entity_meta?: { readonly id?: unknown; readonly project_id?: unknown };
  readonly entity_settings?: { readonly version_id?: unknown; readonly agent_type?: unknown };
}
interface CreateBody {
  readonly source?: string;
  readonly is_private?: boolean;
  readonly meta?: { readonly is_hidden?: boolean };
  readonly participants?: ParticipantRow[];
}

function participantSummary(row: ParticipantRow | undefined): Record<string, unknown> {
  return {
    name: row?.entity_name,
    id: row?.entity_meta?.id,
    projectId: row?.entity_meta?.project_id,
    versionId: row?.entity_settings?.version_id,
    agentType: row?.entity_settings?.agent_type,
  };
}

function installRoutes(): { readonly bodies: CreateBody[] } {
  const bodies: CreateBody[] = [];
  server.use(
    http.post('*/elitea_core/conversations/prompt_lib/:projectId', async ({ request }) => {
      bodies.push((await request.json()) as CreateBody);
      return HttpResponse.json({
        id: 501,
        uuid: '00000000-0000-4000-8000-000000000abc',
        name: 'My Agent',
        source: 'editor_test',
        is_private: true,
        meta: { is_hidden: true, editor_test: { revision: 1, actor_id: '6', project_id: '9', application_id: '42', application_version_id: '7' } },
        participants: [
          { id: '900', entity_name: 'user', entity_meta: { id: 6 } },
          { id: '901', entity_name: 'application', entity_meta: { id: '42', project_id: '9' }, entity_settings: { version_id: '7', agent_type: 'openai' } },
        ],
      });
    }),
    http.get('*/configurations/models/:projectId', () =>
      HttpResponse.json({ items: [{ name: 'gpt-4o', display_name: 'GPT-4o', project_id: '9', default: true }], default_model_name: 'gpt-4o' }),
    ),
    http.get('*/configurations/tts_voices/*', () => HttpResponse.json({ items: [] })),
  );
  return { bodies };
}

function renderPane(node: ReactElement): void {
  renderAgentsRoute(node, '/agents/latest/42', { projectId: '9' });
}

beforeEach(() => {
  globals['elitea_ui_config'] = { vite_server_url: '/api/v2', vite_base_uri: '/', vite_public_project_id: 'public-1' };
  resetConfigForTests();
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  delete globals['elitea_ui_config'];
  resetConfigForTests();
  resetGeneratedClient();
});

describe('AgentTestChat', () => {
  it('creates no conversation until the pane is used', async () => {
    const { bodies } = installRoutes();
    renderPane(<AgentTestChat identity={IDENTITY} user={USER} isDirty={false} />);

    expect(await screen.findByTestId('chat-message-input')).toBeInTheDocument();
    await waitFor(() => expect(bodies).toHaveLength(0));
  });

  it('files the chat as a hidden editor_test conversation that belongs to the agent', async () => {
    const { bodies } = installRoutes();
    const user = userEvent.setup();
    renderPane(<AgentTestChat identity={IDENTITY} user={USER} isDirty={false} />);

    await user.click(await screen.findByTestId('chat-message-input'));

    // One conversation, however many events the one click produced (pointer-down AND focus both call `ensure`).
    await waitFor(() => expect(bodies).toHaveLength(1));
    const body = bodies[0];
    expect(body?.source, 'the marker Main keeps out of the Chats list').toBe('editor_test');
    expect(body?.meta?.is_hidden).toBe(true);
    expect(body?.is_private).toBe(true);
    const rows = body?.participants ?? [];
    expect(rows, 'Main refuses an editor_test create with anything but one application participant').toHaveLength(1);
    expect(participantSummary(rows[0])).toEqual({ name: 'application', id: '42', projectId: '9', versionId: '7', agentType: 'openai' });
  });

  it('tells the author the chat runs the saved version while the form is dirty', async () => {
    installRoutes();
    renderPane(<AgentTestChat identity={IDENTITY} user={USER} isDirty />);

    expect(await screen.findByTestId('agent-test-chat-dirty')).toHaveTextContent('saved version');
    // Not closed: a half-edited agent can still be asked a question.
    expect(screen.getByTestId('chat-message-input')).toBeInTheDocument();
  });

  it("shows Main's own refusal when the conversation cannot be created", async () => {
    installRoutes();
    server.use(
      http.post('*/elitea_core/conversations/prompt_lib/:projectId', () =>
        HttpResponse.json({ code: 'APPLICATION_COMPOSITION_INVALID', error: 'Agent has an attachment that cannot run here.' }, { status: 400 }),
      ),
    );
    const user = userEvent.setup();
    renderPane(<AgentTestChat identity={IDENTITY} user={USER} isDirty={false} />);

    await user.click(await screen.findByTestId('chat-message-input'));

    expect(await screen.findByTestId('agent-test-chat-error')).toHaveTextContent('cannot run here');
  });

  it('does nothing on first use while the version has not loaded', async () => {
    const { bodies } = installRoutes();
    const user = userEvent.setup();
    renderPane(<AgentTestChat identity={{ ...IDENTITY, versionId: undefined }} user={USER} isDirty={false} />);

    await user.click(await screen.findByTestId('chat-message-input'));

    await waitFor(() => expect(bodies).toHaveLength(0));
  });
});
