/**
 * Reading the four settings that decide whether an Inventory toolkit works.
 *
 * Each one fails differently and silently. A blank `bucket` that is not
 * defaulted to `graphs` describes an existing graph as empty. A `sources` list
 * left as NUMBERS never matches `source_configs`, which is keyed by the id as
 * TEXT, so every per-source branch and pattern is lost and the ingestion runs
 * against the whole repository. And a toolkit saved by the legacy UI stores all
 * four under a `toolkit_configuration_` prefix — not reading it makes a
 * configured toolkit read as unconfigured.
 */
import { describe, expect, it } from 'vitest';

import {
  DEFAULT_INVENTORY_BUCKET,
  inventoryBucket,
  inventoryLlmModel,
  inventorySourceConfig,
  inventorySourceIds,
} from './toolkitSettings';

describe('inventoryBucket', () => {
  it('reads the configured bucket', () => {
    expect(inventoryBucket({ bucket: 'inventory-graphs' })).toBe('inventory-graphs');
  });

  it('defaults the way the provider defaults it', () => {
    // `run/compose.go` resolves an unset bucket to `graphs`, so a screen that
    // showed nothing for a blank bucket would be describing a graph that exists.
    expect(inventoryBucket({})).toBe(DEFAULT_INVENTORY_BUCKET);
    expect(inventoryBucket(undefined)).toBe('graphs');
    expect(inventoryBucket({ bucket: '   ' })).toBe('graphs');
  });

  it('reads the legacy toolkit_configuration_ spelling', () => {
    expect(inventoryBucket({ toolkit_configuration_bucket: 'old-graphs' })).toBe('old-graphs');
  });

  it('prefers the bare spelling over the prefixed one', () => {
    expect(
      inventoryBucket({ bucket: 'current', toolkit_configuration_bucket: 'stale' }),
    ).toBe('current');
  });

  it('falls through an empty or null bare value to the prefixed one', () => {
    // An empty bare value is what a form writes when the field was cleared;
    // treating it as an answer hides a value the legacy app really saved.
    expect(inventoryBucket({ bucket: '', toolkit_configuration_bucket: 'old' })).toBe('old');
    expect(inventoryBucket({ bucket: null, toolkit_configuration_bucket: 'old' })).toBe('old');
  });
});

describe('inventoryLlmModel', () => {
  it('reads the model, which is required to ingest and to ask', () => {
    expect(inventoryLlmModel({ llm_model: ' gpt-5 ' })).toBe('gpt-5');
    expect(inventoryLlmModel({ toolkit_configuration_llm_model: 'gpt-4o' })).toBe('gpt-4o');
  });

  it('answers the empty string for a toolkit that reads and cannot ingest', () => {
    // Not a default: an invented model would make the panel offer an ingestion
    // the provider then refuses.
    expect(inventoryLlmModel({})).toBe('');
    expect(inventoryLlmModel(undefined)).toBe('');
    expect(inventoryLlmModel({ llm_model: { name: 'gpt-5' } })).toBe('');
  });
});

describe('inventorySourceIds', () => {
  it('reads numeric ids as TEXT, in saved order', () => {
    // `source_configs` is keyed by the id as a string; comparing 9010 to "9010"
    // silently loses every override.
    expect(inventorySourceIds({ sources: [9010, '9011'] })).toEqual(['9010', '9011']);
  });

  it('drops duplicates, which would render two rows sharing one status', () => {
    expect(inventorySourceIds({ sources: [9010, '9010', 9011] })).toEqual(['9010', '9011']);
  });

  it('drops entries that name nothing', () => {
    expect(inventorySourceIds({ sources: ['', null, {}, ' 9010 '] })).toEqual(['9010']);
  });

  it('reads a missing or non-list value as no sources', () => {
    expect(inventorySourceIds({})).toEqual([]);
    expect(inventorySourceIds(undefined)).toEqual([]);
    expect(inventorySourceIds({ sources: '9010' })).toEqual([]);
  });

  it('reads the legacy prefixed spelling', () => {
    expect(inventorySourceIds({ toolkit_configuration_sources: [1] })).toEqual(['1']);
  });
});

describe('inventorySourceConfig', () => {
  it('reads one source overrides, keyed by the id as text', () => {
    expect(
      inventorySourceConfig(
        {
          source_configs: {
            '9010': {
              branch: 'main',
              file_patterns: '**/*.py',
              exclude_patterns: '**/tests/**',
              preset: 'code',
            },
          },
        },
        '9010',
      ),
    ).toEqual({
      branch: 'main',
      filePatterns: '**/*.py',
      excludePatterns: '**/tests/**',
      preset: 'code',
    });
  });

  it('answers the empty config for a source with nothing saved', () => {
    // Which is the real state of a source just added. Throwing or inventing a
    // branch here would run the ingestion against the wrong tree.
    expect(inventorySourceConfig({ source_configs: {} }, '9010')).toEqual({
      branch: '',
      filePatterns: '',
      excludePatterns: '',
      preset: '',
    });
  });

  it('answers the empty config when source_configs is missing or not an object', () => {
    expect(inventorySourceConfig(undefined, '9010').branch).toBe('');
    expect(inventorySourceConfig({}, '9010').branch).toBe('');
    expect(inventorySourceConfig({ source_configs: null }, '9010').branch).toBe('');
    expect(inventorySourceConfig({ source_configs: [] }, '9010').branch).toBe('');
  });

  it('answers the empty config when the entry is not an object', () => {
    expect(inventorySourceConfig({ source_configs: { '9010': 'main' } }, '9010').branch).toBe('');
    expect(inventorySourceConfig({ source_configs: { '9010': [] } }, '9010').branch).toBe('');
  });

  it('reads the legacy prefixed spelling of the whole map', () => {
    expect(
      inventorySourceConfig(
        { toolkit_configuration_source_configs: { '9010': { branch: 'dev' } } },
        '9010',
      ).branch,
    ).toBe('dev');
  });
});
