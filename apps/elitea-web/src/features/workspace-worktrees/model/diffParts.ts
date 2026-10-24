/**
 * A branch's unified diff (`worktree_diff`) as the `DiffPart[]` the shared
 * `DiffView` paints. Unlike a single file's diff, the file headers stay (as
 * unchanged lines) so the reader can tell which file a hunk belongs to; only
 * `+`/`-` content lines are coloured.
 */
import type { DiffPart } from '@/shared/lib/lineDiff';

const FILE_HEADER = /^(\+\+\+|---) /;

export function diffParts(diff: string): DiffPart[] {
  const parts: { kind: DiffPart['kind']; lines: string[] }[] = [];
  for (const raw of diff.replace(/\n$/, '').split('\n')) {
    const header = FILE_HEADER.test(raw);
    const kind: DiffPart['kind'] = header ? 'unchanged' : raw.startsWith('+') ? 'added' : raw.startsWith('-') ? 'removed' : 'unchanged';
    // The view draws its own marker column, so a content line's own marker (`+`, `-`, or a context line's space) is dropped.
    const text = header || (kind === 'unchanged' && !raw.startsWith(' ')) ? raw : raw.slice(1);
    const last = parts[parts.length - 1];
    if (last !== undefined && last.kind === kind) last.lines.push(text);
    else parts.push({ kind, lines: [text] });
  }
  return parts;
}
