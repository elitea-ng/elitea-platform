/**
 * A chat with two agents whose live presence a journey or a visual shot reads (ADR-0061 decisions 4c and 6,
 * #3067 #3069), against a FIXTURE transcript and a FIXTURE stream.
 *
 * WHY A FIXTURE. Live `agent_state` frames come from a running turn; the offline mock model runs none for long enough
 * to read a label, and a real run's states change by the second. So the page's reads of one conversation are served
 * here (its detail with two agent participants, its messages with one answer still streaming, no traces), and the
 * answer's events come from an `EventSource` that STAYS OPEN: a `page.route` SSE body ends, the stream then ends the
 * run and every avatar goes idle, which is the opposite of what the shot needs. Every other request reaches the real
 * stack.
 */
import type { Page, Route } from '@playwright/test';

const ANSWER_ID = '00000000-0000-4000-8000-000000003067';
const USER_ID = '00000000-0000-4000-8000-000000003066';
const EXECUTION_ID = 'e2e0teampresence000000000000001';

/** Mira (participant 2, a cloud agent in a Local work thread) and CI Doctor (participant 3, this computer's). */
const AGENTS = [
  { id: 2, entity_name: 'application', kind: 'agent', entity_meta: { id: 41, name: 'release-bot' }, persona: { display_name: 'Mira' }, entity_settings: { executor: 'cloud' } },
  { id: 3, entity_name: 'application', kind: 'agent', entity_meta: { id: 42, name: 'CI Doctor' }, entity_settings: {} },
];

/** Mira runs a tool; CI Doctor waits on Mira. */
const AGENT_STATES = [
  { participant_id: 2, state: 'working', activity: { tool: 'jira_get_issue', tool_kind: 'toolkit' } },
  { participant_id: 3, state: 'waiting', activity: { waiting_on: 'agent', peer_participant_id: 2 } },
];

interface TeamPresenceOptions {
  /** A desktop Local work thread opened on the web (read-only, with the lead and where each agent runs). */
  readonly localWork?: boolean;
  /** The answer is still streaming, with `AGENT_STATES` on its stream. */
  readonly live?: boolean;
  /** No tenant holds the conversation (a visual shot creates no server state): its detail is served whole, under this name. */
  readonly servedName?: string;
}

const json = (route: Route, body: unknown) => route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) });

function transcript(conversationId: string, live: boolean) {
  const created = Date.now() - 3 * 24 * 60 * 60_000;
  return {
    items: [
      { id: '9301', uid: USER_ID, role: 'user', content: 'Is the 1.68 release ready?', conversation_id: conversationId, created_at: new Date(created).toISOString() },
      {
        id: '9302',
        uid: ANSWER_ID,
        role: 'assistant',
        content: live ? '' : 'Checking the release ticket with CI Doctor.',
        conversation_id: conversationId,
        created_at: new Date(created + 60_000).toISOString(),
        author_participant_id: 2,
        author_kind: 'agent',
        ...(live ? { is_streaming: true, task_id: EXECUTION_ID } : {}),
      },
    ],
    total: 2,
    page: 0,
    page_size: 50,
    total_pages: 1,
  };
}

/** Replaces `EventSource` for the run's events with one that stays open and delivers `frames` once it is listened to. */
async function openStream(page: Page, frames: readonly Record<string, unknown>[]): Promise<void> {
  await page.addInitScript((serialized: string) => {
    const Real = window.EventSource;
    class HeldOpenEventSource extends EventTarget {
      readonly url: string;
      readonly withCredentials = true;
      readyState = 0;
      onopen: ((event: Event) => void) | null = null;
      onmessage: ((event: MessageEvent) => void) | null = null;
      onerror: ((event: Event) => void) | null = null;
      constructor(url: string) {
        super();
        this.url = url;
        setTimeout(() => {
          this.readyState = 1;
          const open = new Event('open');
          this.dispatchEvent(open);
          this.onopen?.(open);
          (JSON.parse(serialized) as unknown[]).forEach((frame, index) => {
            this.dispatchEvent(new MessageEvent('execution.node_event', { data: JSON.stringify(frame), lastEventId: String(index + 1) }));
          });
        }, 50);
      }
      close(): void {
        this.readyState = 2;
      }
    }
    const Patched = function (this: unknown, url: string | URL, init?: EventSourceInit) {
      const href = String(url);
      return /\/executions\/.+\/events/.test(href) ? new HeldOpenEventSource(href) : new Real(url, init);
    } as unknown as typeof EventSource;
    Object.assign(Patched, { CONNECTING: 0, OPEN: 1, CLOSED: 2 });
    window.EventSource = Patched;
  }, JSON.stringify(frames));
}

/** Serves the page's reads of `conversationId` (a real conversation, so its uuid and rights are the stack's). */
export async function serveTeamPresence(page: Page, conversationId: string, options: TeamPresenceOptions = {}): Promise<void> {
  const live = options.live ?? true;
  const id = (resource: string) => new RegExp(`/api/v2/elitea_core/${resource}/prompt_lib/[^/?#]+/${conversationId}(?:[/?#]|$)`);
  await page.route(id('conversation'), async (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    const real: Record<string, unknown> =
      options.servedName === undefined
        ? ((await (await route.fetch()).json()) as Record<string, unknown>)
        : { id: Number(conversationId), uuid: `00000000-0000-4000-8000-${conversationId.padStart(12, '0')}`, name: options.servedName, is_private: true, participants: [{ id: 1, entity_name: 'user', kind: 'human', entity_meta: { id: 1 }, meta: { user_name: 'Visual Reader' } }], meta: {} };
    const participants = [...((real['participants'] as unknown[] | undefined) ?? []), ...AGENTS];
    const meta = { ...(real['meta'] as Record<string, unknown> | undefined), floor: { policy: 'lead', lead_participant_id: 2 }, ...(options.localWork === true ? { local_work: { folder_name: 'elitea-platform' } } : {}) };
    return json(route, { ...real, participants, meta, ...(options.localWork === true ? { source: 'local_work' } : {}) });
  });
  await page.route(id('messages'), (route) => (route.request().method() === 'GET' ? json(route, transcript(conversationId, live)) : route.fallback()));
  await page.route(/\/elitea_core\/message_traces\/prompt_lib\//, (route) => json(route, { rows: [], total: 0 }));
  if (!live) return;
  await openStream(
    page,
    AGENT_STATES.map((state) => ({ type: 'agent_state', stream_id: conversationId, message_id: ANSWER_ID, ...state })),
  );
}
