/**
 * A turn's background processes (#1223; IPC.md "Background processes"),
 * folded into the `run_command` row that started each: `process_started`
 * marks the row, `tool_output_delta` appends to its output (the last 200
 * lines, escape codes dropped), `process_exited` settles it.
 */
import { plainText } from '@/shared/desktop/processTypes';
import type { AgentEvent } from '@/shared/desktop/workspaceIpc';

import type { ToolProcess, TranscriptItem } from './transcriptItem';

/** How many lines of a process's output a row keeps. */
export const PROCESS_LINES = 200;

type EventOf<K extends AgentEvent['kind']> = Extract<AgentEvent, { kind: K }>;

export type ProcessEvent = EventOf<'process_started'> | EventOf<'tool_output_delta'> | EventOf<'process_exited'>;

export function isProcessEvent(event: AgentEvent): event is ProcessEvent {
  return event.kind === 'process_started' || event.kind === 'tool_output_delta' || event.kind === 'process_exited';
}

function lastLines(text: string): string {
  const lines = text.split('\n');
  return lines.length <= PROCESS_LINES ? text : lines.slice(-PROCESS_LINES).join('\n');
}

const utf8Length = (text: string): number => new TextEncoder().encode(text).length;

function changed(process: ToolProcess | undefined, event: ProcessEvent): ToolProcess {
  const current: ToolProcess = process ?? { processId: event.payload.process_id, status: 'running', output: '', end: 0 };
  switch (event.kind) {
    case 'process_started':
      return current;
    case 'tool_output_delta': {
      const { data, offset } = event.payload;
      const end = offset + utf8Length(data);
      if (end <= current.end) return current; // seen already
      return { ...current, output: lastLines(current.output + plainText(data)), end };
    }
    case 'process_exited': {
      const { status, exit_code: exitCode } = event.payload;
      return { ...current, status, ...(exitCode === undefined ? {} : { exitCode }) };
    }
  }
}

/** `view` with the event folded into the row of its call; an event for a call the turn never showed is dropped. */
export function applyProcessEvent<V extends { items: TranscriptItem[] }>(view: V, event: ProcessEvent): V {
  const callId = event.payload.call_id;
  const index = view.items.findIndex((item) => item.type === 'tool' && item.callId === callId);
  if (index === -1) return view;
  return {
    ...view,
    items: view.items.map((item, i) => (i === index && item.type === 'tool' ? { ...item, process: changed(item.process, event) } : item)),
  };
}
