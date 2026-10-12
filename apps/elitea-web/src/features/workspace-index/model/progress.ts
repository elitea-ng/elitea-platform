/**
 * "Building n of m": the host's `index://event` carries the engine's progress
 * lines as text — `[extract] Parsing <m> files` once the files to read are
 * known, then `[progress] … Processed <n> files …` every ten files. This folds
 * them into numbers; a refresh that has not said how many files it reads yet
 * has `total: null`.
 */
import type { IndexEvent } from '@/shared/desktop/indexIpc';

export interface IndexProgress {
  done: number;
  total: number | null;
}

const PARSING = /Parsing (\d+) files/;
const PROCESSED = /Processed (\d+) files/;

export function foldProgress(previous: IndexProgress | null, event: IndexEvent): IndexProgress | null {
  if (event.status.state !== 'building') return null;
  if (event.phase === 'listing') return { done: 0, total: null };
  const base = previous ?? { done: 0, total: null };
  const message = event.message ?? '';
  const parsing = PARSING.exec(message);
  if (parsing !== null) return { done: 0, total: Number(parsing[1]) };
  const processed = PROCESSED.exec(message);
  if (processed !== null) return { ...base, done: Number(processed[1]) };
  if (event.phase === 'saving' && base.total !== null) return { ...base, done: base.total };
  return base;
}
