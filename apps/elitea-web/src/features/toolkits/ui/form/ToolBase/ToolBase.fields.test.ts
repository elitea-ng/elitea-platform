import { describe, expect, it } from 'vitest';

import {
  isMainPassField,
  isSectionOrToolsField,
  pickSchemaFields,
  resolveRequired,
  schemaEntries,
} from './ToolBase.fields';
import type { ToolSchema } from './types';

const SCHEMA: ToolSchema = {
  properties: {
    label: { type: 'string' },
    url: { type: 'string' },
    scopes: { type: 'array' },
    selected_tools: { type: 'array' },
  },
};

describe('schemaEntries', () => {
  it('returns every property as [key, schema] entries', () => {
    expect(schemaEntries(SCHEMA).map((entry) => entry.key)).toEqual(['label', 'url', 'scopes', 'selected_tools']);
  });

  it('returns an empty array when schema is undefined', () => {
    expect(schemaEntries(undefined)).toEqual([]);
  });
});

describe('pickSchemaFields', () => {
  it('returns entries in the order keys lists them', () => {
    expect(pickSchemaFields(SCHEMA, ['url', 'label']).map((entry) => entry.key)).toEqual(['url', 'label']);
  });

  it('skips keys not present in schema.properties', () => {
    expect(pickSchemaFields(SCHEMA, ['label', 'missing']).map((entry) => entry.key)).toEqual(['label']);
  });
});

describe('isSectionOrToolsField', () => {
  it('is true for selected_tools', () => {
    expect(isSectionOrToolsField('selected_tools', [])).toBe(true);
  });

  it('is true for a field covered by a metadata section', () => {
    expect(isSectionOrToolsField('client_id', ['client_id'])).toBe(true);
  });

  it('is false for an ordinary field', () => {
    expect(isSectionOrToolsField('label', [])).toBe(false);
  });
});

describe('isMainPassField', () => {
  it('excludes a section/tools field', () => {
    expect(isMainPassField('selected_tools', [], [], [], [], [])).toBe(false);
  });

  it('excludes a priority field', () => {
    expect(isMainPassField('url', [], ['url'], [], [], [])).toBe(false);
  });

  it('excludes a bottom field', () => {
    expect(isMainPassField('url', [], [], ['url'], [], [])).toBe(false);
  });

  it('excludes an explicitly-excluded field', () => {
    expect(isMainPassField('url', [], [], [], ['url'], [])).toBe(false);
  });

  it('excludes an advanced field', () => {
    expect(isMainPassField('url', [], [], [], [], ['url'])).toBe(false);
  });

  it('includes an ordinary field in none of the special lists', () => {
    expect(isMainPassField('label', [], [], [], [], [])).toBe(true);
  });
});

describe('resolveRequired', () => {
  it('is true when the key is in schema.required', () => {
    expect(resolveRequired('label', ['label'])).toBe(true);
  });

  it('has no google credential special case (the browser toolkit is gone)', () => {
    expect(resolveRequired('google_api_key', [])).toBe(false);
  });

  it('is false for an ordinary, non-required field', () => {
    expect(resolveRequired('label', [])).toBe(false);
  });
});
