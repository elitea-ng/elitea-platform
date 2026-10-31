import type { OutputContract } from '@/shared/api/generated/model';

/**
 * The output contract of an agent version (`meta.output_contract`) or of a
 * pipeline LLM node (`output_contract`), ADR-0043 decision 2, issue #1500.
 *
 * The server owns the full check (JSON Schema compile, RE2 compile, the size
 * limits) and refuses a bad save with 422 OUTPUT_CONTRACT_INVALID. The editor
 * checks one thing only: that the JSON Schema text parses to an object.
 */

export type OutputContractKind = 'none' | OutputContract['type'];
export type OutputContractFormat = NonNullable<OutputContract['format']>;

/** What the editor holds while a person types. The JSON Schema stays text until it parses. */
export interface OutputContractDraft {
  readonly kind: OutputContractKind;
  readonly schemaText: string;
  readonly pattern: string;
  readonly format: OutputContractFormat;
}

export type OutputContractParse =
  | { readonly ok: true; readonly contract: OutputContract | null }
  | { readonly ok: false; readonly error: 'json' | 'not_object' };

const FORMATS: readonly OutputContractFormat[] = ['json', 'markdown', 'csv'];

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/**
 * Reads a stored contract from an opaque blob (the version `meta` jsonb or a
 * YAML node). A shape the editor cannot show is `null`: the stored value is
 * not touched until the person changes the field.
 */
export function readOutputContract(raw: unknown): OutputContract | null {
  if (!isRecord(raw)) return null;
  const type = raw['type'];
  if (type === 'json_schema' && isRecord(raw['schema'])) return { type, schema: raw['schema'] };
  if (type === 'regex' && typeof raw['pattern'] === 'string') return { type, pattern: raw['pattern'] };
  const format = raw['format'];
  if (type === 'format' && FORMATS.includes(format as OutputContractFormat)) {
    return { type, format: format as OutputContractFormat };
  }
  return null;
}

export function toOutputContractDraft(contract: OutputContract | null | undefined): OutputContractDraft {
  return {
    kind: contract?.type ?? 'none',
    schemaText: contract?.type === 'json_schema' ? JSON.stringify(contract.schema ?? {}, null, 2) : '',
    pattern: contract?.type === 'regex' ? (contract.pattern ?? '') : '',
    format: contract?.type === 'format' ? (contract.format ?? 'json') : 'json',
  };
}

function parseSchema(text: string): OutputContractParse {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return { ok: false, error: 'json' };
  }
  if (!isRecord(parsed)) return { ok: false, error: 'not_object' };
  return { ok: true, contract: { type: 'json_schema', schema: parsed } };
}

/** The draft as the wire shape. `none` is `null`, which clears the stored contract. */
export function fromOutputContractDraft(draft: OutputContractDraft): OutputContractParse {
  switch (draft.kind) {
    case 'none':
      return { ok: true, contract: null };
    case 'json_schema':
      return parseSchema(draft.schemaText);
    case 'regex':
      return { ok: true, contract: { type: 'regex', pattern: draft.pattern } };
    case 'format':
      return { ok: true, contract: { type: 'format', format: draft.format } };
  }
}

/** Key-order-sensitive, which is enough: both sides come from the same reader. */
export function areOutputContractsEqual(a: OutputContract | null | undefined, b: OutputContract | null | undefined): boolean {
  return JSON.stringify(a ?? null) === JSON.stringify(b ?? null);
}
