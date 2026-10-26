/**
 * From the feed's raw facts to what the island shows: a ranked list of
 * activities, most important first. Pure, so the ranking is tested without a
 * DOM.
 *
 * Rank: an approval that is waiting > a turn running > a commit pending or
 * failed > a send to the cloud > a thread running in the cloud > an index
 * build. Nothing running: the idle line, if the caller has one.
 */
import { t } from '@/shared/i18n';

import type { ActivityFeedState, TrackedCloud, TrackedCloudRun, TrackedIndex, TrackedSend, TrackedTurn } from '../model/activityFeed.store';

type ActivityKind = 'approval' | 'turn' | 'commit' | 'handoff' | 'cloud' | 'index';

interface ActivityProgress {
  done: number;
  total: number;
}

export interface Activity {
  /** Stable across updates (a React key and the thing a click acts on). */
  id: string;
  kind: ActivityKind;
  label: string;
  /** The line under the label in the popover: where it runs, how far. */
  detail: string;
  /** `null`: no known total (the bar is indeterminate). */
  progress: ActivityProgress | null;
  /** Epoch ms a running timer counts from. */
  since: number | null;
  tone: 'attention' | 'active' | 'warn';
  workspaceId: string | null;
  conversationId: string | null;
  turnId: string | null;
  /** The action's target for the popover's cancel: a turn id, a workspace id or a conversation uuid. */
  cancelKey: string | null;
}

const RANK: Record<ActivityKind, number> = { approval: 0, turn: 1, commit: 2, handoff: 3, cloud: 4, index: 5 };

const where = (turn: TrackedTurn): string => turn.workspaceName ?? '';

function phaseLabel(turn: TrackedTurn): string {
  if (turn.tool !== null) return t('desktop.island.tool', 'Running {{tool}}', { tool: turn.tool });
  switch (turn.phase) {
    case 'resolving':
    case 'starting':
      return t('desktop.island.starting', 'Starting…');
    case 'committing':
      return t('desktop.island.saving', 'Saving…');
    case 'running':
    case 'done':
    case 'cancelled':
    case 'error':
      return t('desktop.island.working', 'Working…');
  }
}

function turnActivities(turn: TrackedTurn): Activity[] {
  const base = { workspaceId: turn.workspaceId, conversationId: turn.conversationId, turnId: turn.turnId, progress: null, cancelKey: turn.turnId };
  const out: Activity[] = [];
  const [first] = turn.approvals;
  if (first !== undefined) {
    out.push({
      ...base,
      id: `approval:${first.requestId}`,
      kind: 'approval',
      label: t('desktop.island.approve', 'Approve: {{tool}}', { tool: first.tool }),
      detail: [first.title, where(turn)].filter((x) => x !== '').join(' · '),
      since: null,
      tone: 'attention',
    });
  }
  if (turn.commit === 'failed') {
    out.push({ ...base, id: `commit:${turn.turnId}`, kind: 'commit', label: t('desktop.island.commitFailed', 'Not saved'), detail: where(turn), since: null, tone: 'warn', cancelKey: null });
  } else if (turn.commit === 'pending') {
    out.push({ ...base, id: `commit:${turn.turnId}`, kind: 'commit', label: t('desktop.island.saving', 'Saving…'), detail: where(turn), since: null, tone: 'active', cancelKey: null });
  } else if (!turn.finished) {
    const steps = turn.steps === 0 ? '' : turn.steps === 1 ? t('desktop.island.step', '1 step') : t('desktop.island.steps', '{{n}} steps', { n: turn.steps });
    out.push({ ...base, id: `turn:${turn.turnId}`, kind: 'turn', label: phaseLabel(turn), detail: [where(turn), steps].filter((x) => x !== '').join(' · '), since: turn.startedAt, tone: 'active' });
  }
  return out;
}

type FeedFacts = Pick<ActivityFeedState, 'turns' | 'indexes' | 'sends' | 'cloud'> & Partial<Pick<ActivityFeedState, 'cloudRuns'>>;

const NO_TARGET = { workspaceId: null, conversationId: null, turnId: null } as const;

function sendActivity(send: TrackedSend): Activity {
  return {
    ...NO_TARGET,
    id: `handoff:${send.conversationUuid}`,
    kind: 'handoff',
    label: t('desktop.island.sending', 'Sending to cloud · {{phase}}', { phase: send.phase }),
    detail: send.total > 0 ? t('desktop.island.ofFiles', '{{done}} of {{total}}', { done: send.done, total: send.total }) : '',
    progress: send.total > 0 ? { done: send.done, total: send.total } : null,
    since: null,
    tone: 'active',
    cancelKey: send.conversationUuid,
  };
}

function cloudLabel(state: string): string {
  if (state === 'RECLAIMING') return t('desktop.island.returning', 'Returning from cloud');
  return state === 'CLOUD_RUNNING' ? t('desktop.island.cloudRunning', 'Running in cloud') : t('desktop.island.inCloud', 'In cloud');
}

function cloudActivity(cloud: TrackedCloud): Activity {
  return {
    ...NO_TARGET,
    id: `cloud:${cloud.conversationUuid}`,
    kind: 'cloud',
    label: cloudLabel(cloud.state),
    detail: cloud.title,
    progress: null,
    since: null,
    tone: 'active',
    workspaceId: cloud.workspaceId,
    conversationId: cloud.conversationId,
    cancelKey: null,
  };
}

/** A cloud turn streaming into this window ("Running in cloud" with its timer), and/or waiting on the person (an approval). */
function cloudRunActivities(run: TrackedCloudRun): Activity[] {
  const base = { ...NO_TARGET, progress: null, workspaceId: run.workspaceId, conversationId: run.conversationId, cancelKey: null, detail: run.title };
  const out: Activity[] = [];
  if (run.waiting !== null) {
    out.push({ ...base, id: `cloudwait:${run.conversationUuid}`, kind: 'approval', label: run.waiting, since: null, tone: 'attention' });
  }
  if (run.running) {
    out.push({ ...base, id: `cloudrun:${run.conversationUuid}`, kind: 'cloud', label: t('desktop.island.cloudRunning', 'Running in cloud'), since: run.startedAt, tone: 'active' });
  }
  return out;
}

function indexActivity(index: TrackedIndex): Activity {
  const total = index.total ?? 0;
  const known = total > 0;
  return {
    ...NO_TARGET,
    id: `index:${index.workspaceId}`,
    kind: 'index',
    label: known ? t('desktop.island.indexing', 'Indexing {{done}}/{{total}}', { done: index.done, total }) : t('desktop.island.indexingPlain', 'Indexing…'),
    detail: index.workspaceName ?? '',
    progress: known ? { done: index.done, total } : null,
    since: null,
    tone: 'active',
    workspaceId: index.workspaceId,
    cancelKey: index.workspaceId,
  };
}

/** Every live activity, ranked (ties: the one that began first). */
export function buildActivities(state: FeedFacts): Activity[] {
  const list: Activity[] = [
    ...Object.values(state.turns).flatMap(turnActivities),
    ...Object.values(state.sends).map(sendActivity),
    // A thread whose turn streams here shows that (with its timer) instead of the plain "in cloud" line.
    ...Object.values(state.cloudRuns ?? {}).flatMap(cloudRunActivities),
    ...Object.values(state.cloud).filter((cloud) => state.cloudRuns?.[cloud.conversationUuid] === undefined).map(cloudActivity),
    ...Object.values(state.indexes).filter((index) => index.state === 'building').map(indexActivity),
  ];
  return list.sort((a, b) => RANK[a.kind] - RANK[b.kind] || (a.since ?? 0) - (b.since ?? 0));
}

/** `m:ss` (or `h:mm:ss`) of an elapsed time. */
export function formatElapsed(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const s = total % 60;
  const m = Math.floor(total / 60) % 60;
  const h = Math.floor(total / 3600);
  const pad = (n: number): string => String(n).padStart(2, '0');
  return h > 0 ? `${String(h)}:${pad(m)}:${pad(s)}` : `${String(m)}:${pad(s)}`;
}
