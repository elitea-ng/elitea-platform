/* Test-only helper: the workers' golden frames (see client-frames/README.md). */
import { NodeEventSchema, type Frame, type NodeEvent } from '@/features/chat/frames';

const python = require('./client-frames/python.json') as Record<string, unknown>;
const rust = require('./client-frames/rust.json') as Record<string, unknown>;

export type Worker = 'python' | 'rust';

/** A deep copy of one golden frame's `data`, optionally edited. */
export function goldenData(worker: Worker, name: string): Record<string, unknown> {
  const frames = worker === 'python' ? python : rust;
  if (!(name in frames)) throw new Error(`no ${worker} golden frame ${name}`);
  return JSON.parse(JSON.stringify(frames[name])) as Record<string, unknown>;
}

export function nodeOf(data: Record<string, unknown>): NodeEvent {
  return NodeEventSchema.parse(data);
}

export function frameOf(data: Record<string, unknown>): Frame {
  return { kind: 'node', node: nodeOf(data) };
}

export function golden(worker: Worker, name: string, edit?: (data: Record<string, unknown>) => void): Frame {
  const data = goldenData(worker, name);
  edit?.(data);
  return frameOf(data);
}

/** `response_metadata` of a frame's data, for edits. */
export function rmOf(data: Record<string, unknown>): Record<string, unknown> {
  return data.response_metadata as Record<string, unknown>;
}
