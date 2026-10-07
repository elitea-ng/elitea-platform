import type { NativeDevice } from '@/api/queries/devices';
import type { PublicAgent } from '@/api/queries/discover';
import type { Memory } from '@/api/queries/memories';
import type { UsageSummary } from '@/api/queries/usage';
import type { InboxRow } from '@/features/notifications/NotificationsView';
import { formatAge } from '@/lib/time';

/**
 * Sample data for the secondary-screen previews (`/design-secondary`, development builds only).
 * Never sent anywhere; only the fields the real views show (D2). Agents carry no icon, so the
 * previews show the monogram tiles a catalogue without icons gets.
 */

const agent = (id: number, name: string, description: string, category: string | null, tags: string[], agentType = 'openai'): PublicAgent => ({
  id,
  publicProjectId: 1,
  name,
  description,
  versionId: id * 10,
  versionName: 'v1',
  agentType,
  category,
  tags: tags.map((t, i) => ({ id: `${id}-${i}`, name: t })),
  iconPath: null,
  likes: 0,
});

export const SAMPLE_PUBLIC_AGENTS: readonly PublicAgent[] = [
  agent(1, 'Zefir Travel Planner', 'Plans trips with flights, hotels and a day-by-day schedule.', 'Project Management', ['travel', 'planning']),
  agent(2, 'Zefir Concierge', 'Answers questions about the office, benefits and policies.', 'Other', ['hr']),
  agent(3, 'Release Notes Writer', 'Turns merged pull requests into readable release notes.', 'Engineering', ['git', 'docs']),
  agent(4, 'Contract Reviewer', 'Flags unusual clauses and missing terms in contracts.', 'Legal', ['review'], 'pipeline'),
  agent(5, 'Sales Call Prep', 'Builds a one-page brief before a customer call.', 'Sales', []),
  agent(6, 'Bug Triage', 'Groups new issues, finds duplicates and suggests owners.', 'Engineering', ['issues']),
];

export const SAMPLE_WELCOME = 'Hi! Tell me where and when you want to go, and I will plan the trip.';
export const SAMPLE_STARTERS = ['Plan a weekend in Lisbon', 'Find flights to Berlin next Friday', 'What can you do?'];

/** Notifications relative to `now`: a mention today, an approval yesterday, older ones. */
export function sampleInbox(now: number): InboxRow[] {
  const at = (minutes: number) => new Date(now - minutes * 60_000).toISOString();
  const row = (id: number, event_type: string, minutes: number, is_seen: boolean, meta: Record<string, unknown>): InboxRow => ({
    id,
    event_type,
    meta: meta as InboxRow['meta'],
    is_seen,
    created_at: at(minutes),
    age: formatAge(at(minutes), now),
  });
  return [
    row(1, 'chat_user_mentioned', 5, false, { sender: { id: 2, name: 'Bob Zefir' }, conversation_id: '35a5538d-993d-4c66-bf5f-ad8509c581d4' }),
    row(2, 'agent_finished', 50, false, { message: 'Research finished in “Q3 planning”' }),
    row(3, 'moderation_approved', 26 * 60, true, { message: 'Your project request was approved' }),
    row(4, 'user_was_added_to_some_project_as_teammate', 3 * 24 * 60, true, { message: 'You were added to Zefir Team' }),
    row(5, 'chat_user_mentioned', 12 * 24 * 60, true, { sender: { id: 3, name: 'Carol Diaz' }, conversation_id: '11111111-1111-4111-8111-111111111111' }),
  ];
}

export const SAMPLE_MEMORIES: readonly Memory[] = [
  { id: 'm1', content: 'I prefer answers in British English.', tags: ['style'], enabled: true, sourceConversationId: null, updatedAt: '' },
  { id: 'm2', content: 'My team ships on Thursdays.', tags: ['team', 'release'], enabled: true, sourceConversationId: null, updatedAt: '' },
  { id: 'm3', content: 'Keep summaries under five bullet points.', tags: [], enabled: false, sourceConversationId: null, updatedAt: '' },
];

export function sampleDevices(now: number): NativeDevice[] {
  const at = (minutes: number) => new Date(now - minutes * 60_000).toISOString();
  return [
    {
      id: '11111111-1111-4111-8111-111111111111',
      name: 'iPhone',
      platform: 'ios',
      clientVersion: '0.1.0',
      createdAt: at(9000),
      lastSeenAt: at(0),
      current: true,
    },
    {
      id: '22222222-2222-4222-8222-222222222222',
      name: 'Pixel 9',
      platform: 'android',
      clientVersion: '0.1.0',
      createdAt: at(20000),
      lastSeenAt: at(2 * 24 * 60),
      current: false,
    },
    {
      id: '33333333-3333-4333-8333-333333333333',
      name: 'iPad',
      platform: 'ios',
      clientVersion: '0.1.0',
      createdAt: at(40000),
      lastSeenAt: at(9 * 24 * 60),
      current: false,
    },
  ];
}

const usage = (scope: 'user' | 'project', spend: number, limit: number, warning: boolean): UsageSummary => ({
  scope,
  periodStart: '2026-10-01',
  periodEnd: '2026-10-31',
  resetsAt: '2026-11-01T00:00:00Z',
  canSeeAmounts: true,
  unlimited: false,
  spendAvailable: true,
  spend,
  limit,
  remaining: limit - spend,
  percentUsed: (spend / limit) * 100,
  warning,
  totals: { tokens: Math.round(spend * 41000), requests: Math.round(spend * 12) },
});

export const SAMPLE_USAGE = { you: usage('user', 12.4, 50, false), project: usage('project', 462, 500, true) };
