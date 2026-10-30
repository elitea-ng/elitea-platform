import { describe, expect, it } from 'vitest';

import {
  areOutputContractsEqual,
  fromOutputContractDraft,
  readOutputContract,
  toOutputContractDraft,
} from './outputContract';

describe('readOutputContract', () => {
  it.each([
    [{ type: 'json_schema', schema: { type: 'object' } }, { type: 'json_schema', schema: { type: 'object' } }],
    [{ type: 'regex', pattern: '^ok$' }, { type: 'regex', pattern: '^ok$' }],
    [{ type: 'format', format: 'csv' }, { type: 'format', format: 'csv' }],
  ])('reads the stored shape %j', (raw, expected) => {
    expect(readOutputContract(raw)).toEqual(expected);
  });

  it.each([null, undefined, 'regex', [], { type: 'regex' }, { type: 'json_schema', schema: [] }, { type: 'format', format: 'xml' }, { type: 'other' }])(
    'reads %j as no contract',
    (raw) => {
      expect(readOutputContract(raw)).toBeNull();
    },
  );

  it('drops keys that the type does not name', () => {
    expect(readOutputContract({ type: 'regex', pattern: 'a', format: 'json' })).toEqual({ type: 'regex', pattern: 'a' });
  });
});

describe('draft round trip', () => {
  it('gives None for no contract', () => {
    expect(toOutputContractDraft(null).kind).toBe('none');
    expect(fromOutputContractDraft(toOutputContractDraft(undefined))).toEqual({ ok: true, contract: null });
  });

  it.each([
    { type: 'json_schema' as const, schema: { type: 'object', required: ['a'] } },
    { type: 'regex' as const, pattern: '\\d+' },
    { type: 'format' as const, format: 'markdown' as const },
  ])('keeps %j', (contract) => {
    expect(fromOutputContractDraft(toOutputContractDraft(contract))).toEqual({ ok: true, contract });
  });

  it('refuses schema text that is not JSON', () => {
    const draft = { ...toOutputContractDraft(null), kind: 'json_schema' as const, schemaText: '{"type":' };
    expect(fromOutputContractDraft(draft)).toEqual({ ok: false, error: 'json' });
  });

  it('refuses a schema that is not a JSON object', () => {
    const draft = { ...toOutputContractDraft(null), kind: 'json_schema' as const, schemaText: '[1, 2]' };
    expect(fromOutputContractDraft(draft)).toEqual({ ok: false, error: 'not_object' });
  });
});

describe('areOutputContractsEqual', () => {
  it('treats null and undefined as the same', () => {
    expect(areOutputContractsEqual(null, undefined)).toBe(true);
  });

  it('compares by value', () => {
    expect(areOutputContractsEqual({ type: 'regex', pattern: 'a' }, { type: 'regex', pattern: 'a' })).toBe(true);
    expect(areOutputContractsEqual({ type: 'regex', pattern: 'a' }, { type: 'regex', pattern: 'b' })).toBe(false);
  });
});
