/**
 * The host summarized an idle thread in the background, shortly before the
 * provider's prompt cache expired (`workspace-context-compacted`, reason
 * `idle`; `compactionSettings`). The open thread's latest such compaction:
 * the composer's notice says so (`IdleCompactedNotice`), and the context
 * meter shows the smaller conversation until the next model call measures
 * it again (`withIdleCompaction`).
 */
import { useEffect, useState } from 'react';

import { compactionIpc, type ContextCompactedEvent } from '@/shared/desktop/compactionSettings';
import type { ContextMeter } from '@/shared/desktop/workspaceIpc';

export interface IdleCompacted {
  event: ContextCompactedEvent;
  /** When it arrived (RFC 3339). */
  at: string;
}

/** The thread's latest idle compaction this visit, or `null`. */
export function useIdleCompacted(workspaceId: string, conversationId: string): IdleCompacted | null {
  const [compacted, setCompacted] = useState<IdleCompacted | null>(null);
  const [thread, setThread] = useState(`${workspaceId}/${conversationId}`);
  if (thread !== `${workspaceId}/${conversationId}`) {
    setThread(`${workspaceId}/${conversationId}`);
    setCompacted(null);
  }
  useEffect(() => {
    const ipc = compactionIpc();
    if (ipc === undefined || conversationId === '') return undefined;
    let off: (() => void) | undefined;
    let closed = false;
    void ipc
      .onCompacted((event) => {
        if (event.reason !== 'idle' || event.workspace_id !== workspaceId || event.conversation_id !== conversationId) return;
        setCompacted({ event, at: new Date().toISOString() });
      })
      .then((unsubscribe) => {
        if (closed) unsubscribe();
        else off = unsubscribe;
      })
      .catch(() => undefined);
    return () => {
      closed = true;
      off?.();
    };
  }, [workspaceId, conversationId]);
  return compacted;
}

/** `meter` after the idle compaction, unless a model call measured the context since. */
export function withIdleCompaction(meter: ContextMeter | undefined, compacted: IdleCompacted | null): ContextMeter | undefined {
  if (meter === undefined || compacted === null) return meter;
  const measured = meter.measured_at === undefined ? Number.NaN : Date.parse(meter.measured_at);
  if (measured > Date.parse(compacted.at)) return meter;
  const after = compacted.event.after_tokens;
  // The summary replaced older messages: the conversation is what the rest of the request leaves.
  const p = meter.parts;
  const parts = p === undefined ? undefined : { ...p, message_tokens: Math.max(0, after - p.system_tokens - p.tool_tokens - p.mcp_tool_tokens - p.skill_tokens) };
  return {
    ...meter,
    ...(parts === undefined ? {} : { parts }),
    used_tokens: after,
    percent: meter.window_tokens > 0 ? Math.round((after / meter.window_tokens) * 100) : 0,
    phase: 'compacted',
    compacted: true,
    compacted_at: compacted.at,
    measured_at: compacted.at,
  };
}
