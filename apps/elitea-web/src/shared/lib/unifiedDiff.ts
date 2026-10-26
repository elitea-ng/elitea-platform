/**
 * A unified diff (`git diff` text) as the `DiffPart[]` the shared `DiffView`
 * renders. File headers (`diff --git`, `index`, `---`, `+++`) are dropped;
 * hunk headers (`@@ ... @@`) stay as context lines so the reader keeps their
 * place.
 */
import type { DiffPart } from '@/shared/lib/lineDiff';

type Kind = DiffPart['kind'];

const HEADER = /^(diff --git |index |--- |\+\+\+ |new file mode|deleted file mode|similarity index|rename (from|to) |old mode|new mode)/;

function classify(raw: string, inHunk: boolean): { kind: Kind; text: string } | null {
  if (raw.startsWith('@@') || raw.startsWith('\\')) return { kind: 'unchanged', text: raw };
  if (!inHunk && HEADER.test(raw)) return null;
  if (raw.startsWith('+')) return { kind: 'added', text: raw.slice(1) };
  if (raw.startsWith('-')) return { kind: 'removed', text: raw.slice(1) };
  if (!inHunk) return null;
  return { kind: 'unchanged', text: raw.startsWith(' ') ? raw.slice(1) : raw };
}

export function parseUnifiedDiff(diff: string): DiffPart[] {
  const parts: { kind: Kind; lines: string[] }[] = [];
  let inHunk = false;
  for (const raw of diff.split('\n')) {
    const line = classify(raw, inHunk);
    if (line === null) continue;
    if (raw.startsWith('@@')) inHunk = true;
    const last = parts[parts.length - 1];
    if (last !== undefined && last.kind === line.kind) last.lines.push(line.text);
    else parts.push({ kind: line.kind, lines: [line.text] });
  }
  // A trailing newline in the input leaves one empty context line at the end.
  const tail = parts[parts.length - 1];
  if (tail !== undefined && tail.kind === 'unchanged' && tail.lines[tail.lines.length - 1] === '') {
    tail.lines.pop();
    if (tail.lines.length === 0) parts.pop();
  }
  return parts;
}
