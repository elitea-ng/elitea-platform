import { describe, expect, it } from 'vitest';

import { isIndexConfigDirty, pickIndexConfigValues } from './indexConfigDirty';

const KEYS = ['index_name', 'clean_index', 'progress_step', 'chunking_config'];

describe('pickIndexConfigValues', () => {
  it('keeps only the keys the current tool schema declares', () => {
    expect(
      pickIndexConfigValues(KEYS, { index_name: 'docs', progress_step: 50, retired_option: true }),
    ).toEqual({ index_name: 'docs', progress_step: 50 });
  });

  it('is an empty object when nothing is stored yet', () => {
    expect(pickIndexConfigValues(KEYS, undefined)).toEqual({});
  });

  it('keeps a key stored as null — an explicit clear is a stored value, not an absent one', () => {
    expect(pickIndexConfigValues(KEYS, { progress_step: null })).toEqual({ progress_step: null });
  });
});

describe('isIndexConfigDirty', () => {
  const saved = { index_name: 'docs', clean_index: false, progress_step: 50 };

  it('is clean when the form still holds the saved values', () => {
    expect(isIndexConfigDirty({ schemaKeys: KEYS, current: { ...saved }, saved })).toBe(false);
  });

  it('is dirty when a value changes', () => {
    expect(isIndexConfigDirty({ schemaKeys: KEYS, current: { ...saved, progress_step: 75 }, saved })).toBe(true);
  });

  it('goes clean again when the value is changed back — the case the buttons hang on (ELITEA-2883 step 9)', () => {
    const edited = { ...saved, progress_step: 75 };
    expect(isIndexConfigDirty({ schemaKeys: KEYS, current: edited, saved })).toBe(true);
    expect(isIndexConfigDirty({ schemaKeys: KEYS, current: { ...edited, progress_step: 50 }, saved })).toBe(false);
  });

  it('tracks several fields at once', () => {
    expect(
      isIndexConfigDirty({ schemaKeys: KEYS, current: { ...saved, clean_index: true, progress_step: 75 }, saved }),
    ).toBe(true);
  });

  it('compares the chunking-config object STRUCTURALLY — a fresh object with the same contents is not a change', () => {
    const withObject = { ...saved, chunking_config: { '.pdf': { max_tokens: 512 } } };
    expect(
      isIndexConfigDirty({
        schemaKeys: KEYS,
        current: { ...saved, chunking_config: { '.pdf': { max_tokens: 512 } } },
        saved: withObject,
      }),
    ).toBe(false);
    expect(
      isIndexConfigDirty({
        schemaKeys: KEYS,
        current: { ...saved, chunking_config: { '.pdf': { max_tokens: 1024 } } },
        saved: withObject,
      }),
    ).toBe(true);
  });

  it('does not care about key ORDER inside an object', () => {
    expect(
      isIndexConfigDirty({
        schemaKeys: ['chunking_config'],
        current: { chunking_config: { b: 2, a: 1 } },
        saved: { chunking_config: { a: 1, b: 2 } },
      }),
    ).toBe(false);
  });

  it('treats undefined, null, the empty string, [] and {} as the same "holds nothing" state', () => {
    // The form's own default resolution produces `null`, `[]` and `{}` for
    // keys the stored configuration simply omits. Without this, every index
    // would open already dirty — see `isEmptyConfigValue`'s own note.
    const empties: unknown[] = [undefined, null, '', [], {}];
    for (const current of empties) {
      for (const stored of empties) {
        expect(isIndexConfigDirty({ schemaKeys: ['folder'], current: { folder: current }, saved: { folder: stored } })).toBe(false);
      }
    }
    expect(isIndexConfigDirty({ schemaKeys: ['folder'], current: { folder: 'docs' }, saved: {} })).toBe(true);
  });

  it('still calls CLEARING a non-empty list a change', () => {
    expect(
      isIndexConfigDirty({
        schemaKeys: ['include_extensions'],
        current: { include_extensions: [] },
        saved: { include_extensions: ['*.png'] },
      }),
    ).toBe(true);
  });

  it('ignores keys outside the schema — a retired stored option cannot pin the form dirty', () => {
    expect(
      isIndexConfigDirty({ schemaKeys: KEYS, current: { ...saved }, saved: { ...saved, retired_option: true } }),
    ).toBe(false);
  });

  it('is clean for an index with no schema properties at all', () => {
    expect(isIndexConfigDirty({ schemaKeys: [], current: { anything: 1 }, saved: {} })).toBe(false);
  });

  it('distinguishes 0 and false from "holds nothing"', () => {
    expect(isIndexConfigDirty({ schemaKeys: ['progress_step'], current: { progress_step: 0 }, saved: {} })).toBe(true);
    expect(isIndexConfigDirty({ schemaKeys: ['clean_index'], current: { clean_index: false }, saved: {} })).toBe(true);
  });
});
