/**
 * DEV ONLY — the desktop harness's canned data (`devHarness.tsx`): the folders, the Chats list, the archived chats,
 * the chat stand-in with its participants, and the API stub that answers the few calls the workspace screens make.
 */
import Typography from '@mui/material/Typography';

import { ChatParticipantsPanel } from '@/pages/chat/ChatParticipantsPanel';
import type { Workspace } from '@/shared/desktop/workspaceIpc';

import { floorAnswer } from './devHarnessFloor';

export const FOLDERS: Workspace[] = [
  { id: 'w1', path: '/Users/me/code/elitea-platform', name: 'elitea-platform', project_id: 42, is_git: true },
  { id: 'w2', path: '/Users/me/notes/docs', name: 'docs', project_id: null, is_git: false },
];

const hoursAgo = (hours: number): string => new Date(Date.now() - hours * 3_600_000).toISOString();
type ChatActivity = { readonly state: 'running' | 'needs_you' | 'idle'; readonly unread?: boolean };
const chat = (id: string, name: string, hours: number, activity?: ChatActivity) => ({
  id,
  name,
  is_private: true,
  author_id: 3,
  updated_at: hoursAgo(hours),
  created_at: hoursAgo(hours),
  // Client contract 1.12: the row's activity status.
  ...(activity === undefined ? {} : { activity: { state: activity.state, since: hoursAgo(hours) }, last_activity_at: hoursAgo(hours), unread: activity.unread === true }),
});

/** The Chats list (`&chats`): the grouped folders listing the sidebar reads. */
const GROUPED_CHATS = {
  pinned: { conversations: [chat('81', 'Release checklist', 30)] },
  folders: [{ id: '5', name: 'Research', meta: { is_pinned: false }, total: 2, offset: 2, conversations: [chat('91', 'Vector stores compared', 50), chat('92', 'Embedding model picks', 70)] }],
  date_groups: [
    {
      name: 'Today',
      total: 12,
      offset: 4,
      needs_you: 1,
      conversations: [
        chat('72', 'quick overview', 0, { state: 'running' }),
        chat('73', 'Fix flaky gateway test', 1, { state: 'needs_you' }),
        chat('74', 'Draft the Q4 roadmap email', 2, { state: 'idle', unread: true }),
        chat('79', 'Rename the config keys', 3, { state: 'idle' }),
      ],
    },
    { name: 'Yesterday', total: 2, offset: 2, conversations: [chat('75', 'Postgres connection math', 26, { state: 'idle', unread: true }), chat('76', 'Explain ADR-0034', 28)] },
    { name: 'This week', total: 1, offset: 1, conversations: [chat('77', 'Helm values for staging', 80)] },
    { name: 'This month', total: 0, offset: 0, conversations: [] },
    { name: 'Older', total: 1, offset: 1, conversations: [chat('78', 'Onboarding notes', 900)] },
  ],
  total_folders: 1,
};

const ARCHIVED_CHATS = {
  total: 2,
  rows: [
    { id: 61, name: 'Old sprint retro', created_at: hoursAgo(400), updated_at: hoursAgo(300), duration: -1, message_groups_count: 4, is_pinned: false, is_archived: true, archived_by: { id: 4, name: 'Ana Ruiz' }, archived_at: hoursAgo(5) },
    { id: 62, name: 'Spike: socket.io removal', created_at: hoursAgo(900), updated_at: hoursAgo(800), duration: -1, message_groups_count: 9, is_pinned: false, is_archived: true, archived_by: { id: 3, name: 'Me' }, archived_at: hoursAgo(50) },
  ],
};

/** The participants the harness chat's inspector shows. */
const HARNESS_CONVERSATION = {
  id: '72',
  name: 'quick overview',
  participants: [
    { id: 3, entity_name: 'application', entity_meta: { id: 5, project_id: 42 }, entity_settings: { agent_type: 'openai' }, meta: { name: 'Coder' } },
    { id: 4, entity_name: 'application', entity_meta: { id: 6, project_id: 42 }, entity_settings: { agent_type: 'openai' }, meta: { name: 'Reviewer' } },
    { id: 7, entity_name: 'user', entity_meta: { id: 3 }, meta: { name: 'Me' } },
  ],
};

export function HarnessChat({ title }: { title: string }): React.JSX.Element {
  return (
    <>
      <Placeholder title={title} />
      <ChatParticipantsPanel
        activeConversation={HARNESS_CONVERSATION}
        activeParticipant={undefined}
        projectId="42"
        isMcpVisible={false}
        handlers={{ onSelectParticipant: () => undefined, onDeleteParticipant: () => undefined, onEditParticipant: () => undefined, onAddParticipants: () => undefined }}
      />
    </>
  );
}

const API: [string, RegExp, unknown][] = [
  ['GET', /\/elitea_core\/folder\/prompt_lib\/42$/, GROUPED_CHATS],
  ['PUT', /\/elitea_core\/conversation_archive\/prompt_lib\/42\/\d+$/, { ok: true }],
  ['DELETE', /\/elitea_core\/conversation_archive\/prompt_lib\/42\/\d+$/, { ok: true }],
  ['PUT', /\/elitea_core\/conversation\/prompt_lib\/42\/\d+$/, { id: 72 }],
  ['DELETE', /\/elitea_core\/conversation\/prompt_lib\/42\/\d+$/, {}],
  ['POST', /\/social\/pin\//, { ok: true }],
  ['DELETE', /\/social\/pin\//, { ok: true }],
  ['GET', /\/projects\/project\/default\//, [{ id: '1', name: 'Public', suspended: false }, { id: '42', name: 'Platform team', suspended: false }]],
  ['GET', /\/elitea_core\/applications\/prompt_lib\/42/, { rows: [{ id: '5', name: 'Coder', tags: [] }, { id: '6', name: 'Reviewer', tags: [] }], total: 2 }],
  [
    'GET',
    /\/elitea_core\/application\/prompt_lib\/42\/\d+/,
    { id: '5', name: 'Coder', description: '', versions: [{ id: '9', name: 'base', status: 'published', agent_type: 'openai' }] },
  ],
  ['POST', /\/elitea_core\/conversations\/prompt_lib\/42$/, { id: 77, name: 'quick overview' }],
  ['GET', /\/elitea_core\/conversation\/prompt_lib\/42\/\d+$/, { id: 72, uuid: 'c0ffee00-0000-4000-8000-000000000072', name: 'quick overview', participants: [{ id: 3, entity_name: 'application', name: 'Coder', meta: {} }] }],
  ['POST', /\/elitea_core\/continue_predict\/prompt_lib\/42\/c0ffee00-0000-4000-8000-000000000072$/, { events_url: '/api/v2/executions/42/exec-1/events', response_message_id: 'e3f5b0f2-8a3c-4d9a-9a1e-0c2b7f5d1a44' }],
  // The cloud copy's messages (`&cloud`), and the start route a message to it goes through.
  ['GET', /\/elitea_core\/messages\/prompt_lib\/42\/72$/, { items: [{ id: 'm1', uid: 'm1', role: 'user', content: 'Give me a quick overview of the repo', conversation_id: '72', created_at: '2026-10-20T08:00:00Z' }, { id: 'm2', uid: 'm2', role: 'assistant', content: 'The repository is a **Go monorepo** with the desktop app in `apps/elitea-desktop`.', conversation_id: '72', created_at: '2026-10-20T08:00:05Z' }], total: 2, page: 1, page_size: 100, total_pages: 1 }],
  ['POST', /\/elitea_core\/messages\/prompt_lib\/42\/c0ffee00-0000-4000-8000-000000000072$/, { task_id: 'exec-1', events_url: '/api/v2/executions/42/exec-1/events', response_message_id: 'e3f5b0f2-8a3c-4d9a-9a1e-0c2b7f5d1a44' }],
  ['GET', /\/elitea_core\/conversations\/prompt_lib\/42/, { rows: [], total: 0 }],
  ['POST', /\/elitea_core\/participants\//, []],
  ['GET', /\/social\/author/, { id: 3, name: 'Alex K', email: 'alex@example.com' }],
  ['GET', /\/auth\/permissions\/prompt_lib\/42$/, ['get', 'create', 'update', 'delete'].map((verb) => ({ name: `models.chat.folders.${verb}`, enabled: true }))],
];

export function installApiStub(): void {
  const answer = (method: string, url: string, body?: unknown): Response => {
    const parsed = new URL(url, window.location.origin);
    const path = parsed.pathname;
    const floor = floorAnswer(method, path, body); // several agents in a thread (`devHarnessFloor.ts`)
    if (floor !== undefined) return floor;
    if (method === 'GET' && parsed.searchParams.get('archived') === 'only') {
      return new Response(JSON.stringify(ARCHIVED_CHATS), { status: 200, headers: { 'content-type': 'application/json' } });
    }
    const hit = API.find(([verb, pattern]) => verb === method && pattern.test(path));
    return new Response(JSON.stringify(hit === undefined ? {} : hit[2]), {
      status: hit === undefined ? 404 : 200,
      headers: { 'content-type': 'application/json' },
    });
  };
  // oxlint-disable-next-line no-restricted-properties -- dev-only harness: stands in for the deployment the generated client would call.
  window.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
    const request = input instanceof Request ? input : undefined;
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    return Promise.resolve(answer((init?.method ?? request?.method ?? 'GET').toUpperCase(), url, init?.body));
  };
}

export function Placeholder({ title }: { title: string }): React.JSX.Element {
  return (
    <Typography variant="headingMedium" component="h1" sx={{ padding: 3 }}>
      {title}
    </Typography>
  );
}
