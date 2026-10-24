/**
 * The Context tab's figures (#1217): what fills the model's window at the
 * thread's latest model call, as the host measured it (IPC.md "Context
 * window"). The rows add up to the window: what the request carries (by
 * part, when the host reports the parts), the compaction's buffer (the
 * window past the point where the host compacts) and the free space.
 * Estimates, never content.
 */
import type { WorkspaceTurn } from '@/features/workspace';
import type { AgentEvent, ContextMeter } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import type { ThreadHistory } from './useThreadHistory';

/** At or past this share of the window the figures turn to a warning. */
export const WARN_PERCENT = 80;

export type ContextRowKey = 'system' | 'tools' | 'mcp' | 'skills' | 'messages' | 'used' | 'buffer' | 'free';

export interface ContextRow {
  key: ContextRowKey;
  label: string;
  tokens: number;
  /** Of the window, 0 to 100. */
  percent: number;
}

export interface ContextBreakdown {
  /** Used of the window, 0 to 100, rounded. */
  percent: number;
  usedTokens: number;
  windowTokens: number;
  /** What the request carries, then the buffer and the free space (rows of 0 tokens are left out, but free space). */
  rows: ContextRow[];
}

/** Tokens as "20.3K". */
export const formatTokens = (value: number): string => new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 }).format(value);

/** A share of the window as "2.0%". */
export const formatPercent = (value: number): string => `${value.toFixed(1)}%`;

function usedRows(meter: ContextMeter): Omit<ContextRow, 'percent'>[] {
  const parts = meter.parts;
  if (parts === undefined) return [{ key: 'used', label: t('workspace.context.row.used', 'In use'), tokens: meter.used_tokens }];
  return [
    { key: 'system', label: t('workspace.context.row.system', 'System prompt'), tokens: parts.system_tokens },
    { key: 'tools', label: t('workspace.context.row.tools', 'Tools'), tokens: parts.tool_tokens },
    { key: 'mcp', label: t('workspace.context.row.mcp', 'MCP tools'), tokens: parts.mcp_tool_tokens },
    { key: 'skills', label: t('workspace.context.row.skills', 'Skills'), tokens: parts.skill_tokens },
    { key: 'messages', label: t('workspace.context.row.messages', 'Messages'), tokens: parts.message_tokens },
  ];
}

export function contextBreakdown(meter: ContextMeter): ContextBreakdown {
  const window = meter.window_tokens;
  const used = meter.used_tokens;
  const compactAt = meter.compact_at_tokens !== undefined && meter.compact_at_tokens > 0 && meter.compact_at_tokens < window ? meter.compact_at_tokens : window;
  const share = (tokens: number): number => (window > 0 ? Math.min(100, (tokens / window) * 100) : 0);
  const rows = [
    ...usedRows(meter).filter((row) => row.tokens > 0),
    { key: 'buffer' as const, label: t('workspace.context.row.buffer', 'Compaction buffer'), tokens: Math.max(0, window - Math.max(compactAt, used)) },
  ].filter((row) => row.tokens > 0);
  rows.push({ key: 'free', label: t('workspace.context.row.free', 'Free space'), tokens: Math.max(0, compactAt - used) });
  return {
    percent: Math.round(share(used)),
    usedTokens: used,
    windowTokens: window,
    rows: rows.map((row) => ({ ...row, percent: share(row.tokens) })),
  };
}

/** The newest meter of `events` (a recorded turn's), from its `usage` and `context` events. */
export function meterOfEvents(events: readonly AgentEvent[]): ContextMeter | undefined {
  for (let at = events.length - 1; at >= 0; at -= 1) {
    const event = events[at];
    if (event?.kind === 'context') return event.payload;
    if (event?.kind === 'usage' && event.payload.context !== undefined) return event.payload.context;
  }
  return undefined;
}

/** "Updated 2 minutes ago" for a measurement at `at` (RFC 3339), seen at `now`. */
export function updatedAgo(at: string | undefined, now: number): string | null {
  if (at === undefined) return null;
  const then = Date.parse(at);
  if (Number.isNaN(then)) return null;
  const minutes = Math.floor(Math.max(0, now - then) / 60_000);
  if (minutes < 1) return t('workspace.context.updatedNow', 'Updated just now');
  if (minutes < 60) return t('workspace.context.updatedMinutes', 'Updated {{count}} min ago', { count: minutes });
  return t('workspace.context.updatedAt', 'Updated at {{time}}', { time: new Date(then).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }) });
}

/**
 * The thread's latest context occupancy: the shown turn's, else the newest earlier one of this visit, else the newest
 * recorded turn's (so the tab has figures after a restart).
 */
export function threadMeter(turn: WorkspaceTurn, history: ThreadHistory): ContextMeter | undefined {
  const live = turn.view.context ?? turn.earlier.findLast((earlier) => earlier.view.context !== undefined)?.view.context;
  if (live !== undefined || history.kind !== 'local') return live;
  for (const stored of [...history.turns].reverse()) {
    const meter = meterOfEvents(stored.events);
    if (meter !== undefined) return meter;
  }
  return undefined;
}
