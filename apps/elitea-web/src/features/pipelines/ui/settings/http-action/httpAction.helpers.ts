export interface HttpStatusRange {
  readonly first: number;
  readonly last: number;
}

export interface HttpBindingRow {
  readonly name: string;
  readonly source: 'from' | 'value';
  readonly value: string;
  readonly optional: boolean;
}

export interface HttpBodyDraft {
  readonly kind: 'none' | 'json' | 'text' | 'form';
  readonly source: 'from' | 'value';
  readonly value: string;
  readonly contentType: string;
}

export interface HttpIdempotencyDraft {
  readonly mode: 'none' | 'from' | 'value';
  readonly value: string;
}

export function asRecord(value: unknown): Readonly<Record<string, unknown>> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? (value as Readonly<Record<string, unknown>>)
    : {};
}

export function formatAcceptedStatuses(raw: unknown): string {
  if (!Array.isArray(raw) || raw.length === 0) return '200-299';
  const parts: string[] = [];
  for (const item of raw) {
    const rec = asRecord(item);
    const first = rec['first'];
    const last = rec['last'];
    if (typeof first === 'number' && typeof last === 'number') {
      parts.push(first === last ? String(first) : `${first}-${last}`);
    }
  }
  return parts.length > 0 ? parts.join(', ') : '200-299';
}

export function parseAcceptedStatuses(text: string): readonly HttpStatusRange[] | undefined {
  const tokens = text
    .split(',')
    .map((part) => part.trim())
    .filter(Boolean);
  if (tokens.length === 0) return undefined;
  const ranges: HttpStatusRange[] = [];
  for (const token of tokens) {
    const match = /^(\d{3})(?:\s*-\s*(\d{3}))?$/.exec(token);
    if (!match) return undefined;
    const first = Number(match[1]);
    const last = match[2] !== undefined ? Number(match[2]) : first;
    if (first < 200 || last > 599 || first > last) return undefined;
    ranges.push({ first, last });
  }
  return ranges.length > 0 && ranges.length <= 16 ? ranges : undefined;
}

function scalarToText(value: unknown): string {
  return typeof value === 'string' || typeof value === 'number' || typeof value === 'boolean' ? String(value) : '';
}

export function readBindingMapRows(raw: unknown): readonly HttpBindingRow[] {
  const record = asRecord(raw);
  return Object.entries(record).map(([name, entry]) => {
    const binding = asRecord(entry);
    if (typeof binding['from'] === 'string') {
      return { name, source: 'from', value: binding['from'], optional: binding['optional'] === true };
    }
    return {
      name,
      source: 'value',
      value: scalarToText(binding['value']),
      optional: false,
    };
  });
}

export function writeBindingMapRows(rows: readonly HttpBindingRow[]): Record<string, unknown> | undefined {
  const next: Record<string, unknown> = {};
  for (const row of rows) {
    const key = row.name.trim();
    if (!key) continue;
    if (row.source === 'from') {
      next[key] = row.optional ? { from: row.value.trim(), optional: true } : { from: row.value.trim() };
    } else {
      next[key] = { value: row.value };
    }
  }
  return Object.keys(next).length > 0 ? next : undefined;
}

export function readHeaderRows(raw: unknown): readonly HttpBindingRow[] {
  if (!Array.isArray(raw)) return [];
  return raw.map((entry) => {
    const item = asRecord(entry);
    const name = typeof item['name'] === 'string' ? item['name'] : '';
    if (typeof item['from'] === 'string') {
      return { name, source: 'from', value: item['from'], optional: item['optional'] === true };
    }
    return {
      name,
      source: 'value',
      value: scalarToText(item['value']),
      optional: false,
    };
  });
}

export function writeHeaderRows(rows: readonly HttpBindingRow[]): readonly Record<string, unknown>[] | undefined {
  const next: Record<string, unknown>[] = [];
  for (const row of rows) {
    const name = row.name.trim();
    if (!name) continue;
    if (row.source === 'from') {
      next.push(row.optional ? { name, from: row.value.trim(), optional: true } : { name, from: row.value.trim() });
    } else {
      next.push({ name, value: row.value });
    }
  }
  return next.length > 0 ? next : undefined;
}

export function readBodyDraft(raw: unknown): HttpBodyDraft {
  const body = asRecord(raw);
  const kind = body['kind'];
  if (kind !== 'json' && kind !== 'text' && kind !== 'form') {
    return { kind: 'none', source: 'from', value: '', contentType: '' };
  }
  const contentType = typeof body['content_type'] === 'string' ? body['content_type'] : '';
  if (typeof body['from'] === 'string') {
    return { kind, source: 'from', value: body['from'], contentType };
  }
  const rawVal = body['value'];
  const value = typeof rawVal === 'string' ? rawVal : rawVal === undefined ? '' : JSON.stringify(rawVal);
  return { kind, source: 'value', value, contentType };
}

export function writeBodyDraft(draft: HttpBodyDraft): Record<string, unknown> | undefined {
  if (draft.kind === 'none') return undefined;
  const next: Record<string, unknown> = { kind: draft.kind };
  if (draft.source === 'from') {
    next['from'] = draft.value.trim();
  } else if (draft.kind === 'json') {
    try {
      next['value'] = JSON.parse(draft.value);
    } catch {
      next['value'] = draft.value;
    }
  } else {
    next['value'] = draft.value;
  }
  if (draft.contentType.trim() !== '') {
    next['content_type'] = draft.contentType.trim();
  }
  return next;
}

export function readIdempotencyDraft(raw: unknown): HttpIdempotencyDraft {
  const rec = asRecord(raw);
  if (typeof rec['from'] === 'string') return { mode: 'from', value: rec['from'] };
  if (rec['value'] !== undefined && rec['value'] !== null) return { mode: 'value', value: scalarToText(rec['value']) };
  return { mode: 'none', value: '' };
}

export function writeIdempotencyDraft(draft: HttpIdempotencyDraft): Record<string, unknown> | undefined {
  if (draft.mode === 'from') return { from: draft.value.trim() };
  if (draft.mode === 'value') return { value: draft.value };
  return undefined;
}
