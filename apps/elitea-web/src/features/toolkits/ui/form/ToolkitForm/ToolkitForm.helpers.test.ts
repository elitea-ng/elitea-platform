import { describe, expect, it, vi } from 'vitest';

import { ConfigurationMode } from '@/entities/toolkit';

import type { ConfigurationWire } from '../../../api/configurations';

import {
  applyAutoSelectFormReset,
  resolveCredentialReverts,
  resolveDisabledConfigFields,
  resolveExcludedFields,
  resolveIsLoading,
  resolveOutOfBandFieldSync,
  resolveSupportsConfiguration,
  updateDetailByPath,
} from './ToolkitForm.helpers';
import type { ToolkitConfigurationState, ToolkitFormEditDetail } from './ToolkitForm.types';

describe('updateDetailByPath', () => {
  it('sets a single-segment path with a scalar value', () => {
    expect(updateDetailByPath({ name: 'old' }, 'name', 'new')).toEqual({ name: 'new' });
  });

  it('merges a single-segment object value into an existing object value (default replace=false)', () => {
    const result = updateDetailByPath({ settings: { a: 1, b: 2 } }, 'settings', { b: 3, c: 4 });
    expect(result).toEqual({ settings: { a: 1, b: 3, c: 4 } });
  });

  it('replaces (does not merge) a single-segment object value when replace=true', () => {
    const result = updateDetailByPath({ settings: { a: 1, b: 2 } }, 'settings', { c: 4 }, true);
    expect(result).toEqual({ settings: { c: 4 } });
  });

  it('does not merge when the existing value is not an object (overwrites instead)', () => {
    const result = updateDetailByPath({ name: 'old' }, 'name', { nested: true });
    expect(result).toEqual({ name: { nested: true } });
  });

  it('does not merge when the new value is not an object (overwrites instead)', () => {
    const result = updateDetailByPath({ settings: { a: 1 } }, 'settings', 'plain-string');
    expect(result).toEqual({ settings: 'plain-string' });
  });

  it('does not merge when the existing value is null', () => {
    // `settings` is typed `Record<string, unknown> | undefined` (no `null`) —
    // a real caller can still hand this a `null` at runtime through the
    // index-signature-typed `[key: string]: unknown` escape hatch, so the
    // cast documents an intentionally off-type input, not a type error.
    const detail = { settings: null } as unknown as ToolkitFormEditDetail;
    const result = updateDetailByPath(detail, 'settings', { a: 1 });
    expect(result).toEqual({ settings: { a: 1 } });
  });

  it('replaces a nested array without changing it into an object', () => {
    const detail: ToolkitFormEditDetail = { settings: { selected_tools: [] } };
    const result = updateDetailByPath(detail, 'settings.selected_tools', ['echo_marker']);
    expect(result).toEqual({ settings: { selected_tools: ['echo_marker'] } });
    expect(Array.isArray(result.settings?.selected_tools)).toBe(true);
  });

  it('sets a nested dot-path field, creating intermediate objects as needed', () => {
    const result = updateDetailByPath({}, 'meta.mcp_options', { url: 'https://x' });
    expect(result).toEqual({ meta: { mcp_options: { url: 'https://x' } } });
  });

  it('recurses through a nested path, merging at the leaf', () => {
    const detail: ToolkitFormEditDetail = { meta: { mcp_options: { url: 'a', scope: 'b' } } };
    const result = updateDetailByPath(detail, 'meta.mcp_options', { url: 'c' });
    expect(result).toEqual({ meta: { mcp_options: { url: 'c', scope: 'b' } } });
  });

  it('recurses through a three-segment path', () => {
    const result = updateDetailByPath({}, 'a.b.c', 'leaf');
    expect(result).toEqual({ a: { b: { c: 'leaf' } } });
  });

  it('does not mutate the original detail object', () => {
    const detail: ToolkitFormEditDetail = { settings: { a: 1 } };
    const result = updateDetailByPath(detail, 'settings', { b: 2 });
    expect(detail).toEqual({ settings: { a: 1 } });
    expect(result).not.toBe(detail);
  });
});

describe('resolveExcludedFields', () => {
  it('excludes discovery fields for the mcp tool type', () => {
    expect(resolveExcludedFields('mcp')).toEqual(['discovery_mode', 'discovery_interval']);
  });

  it('excludes nothing for a non-mcp tool type', () => {
    expect(resolveExcludedFields('github')).toEqual([]);
  });

  it('excludes nothing for an empty tool type', () => {
    expect(resolveExcludedFields('')).toEqual([]);
  });
});

describe('resolveSupportsConfiguration', () => {
  it('returns true when a plain-string integration entry matches "integration_<toolType>"', () => {
    expect(resolveSupportsConfiguration(['integration_github', 'integration_jira'], 'github')).toBe(true);
  });

  it('returns false when no plain-string integration entry matches', () => {
    expect(resolveSupportsConfiguration(['integration_jira'], 'github')).toBe(false);
  });

  it('returns true when a ConfigurationWire object entry\'s .type matches', () => {
    const integrations: readonly ConfigurationWire[] = [{ type: 'integration_github' }];
    expect(resolveSupportsConfiguration(integrations, 'github')).toBe(true);
  });

  it('returns false when a ConfigurationWire object entry\'s .type does not match', () => {
    const integrations: readonly ConfigurationWire[] = [{ type: 'integration_jira' }];
    expect(resolveSupportsConfiguration(integrations, 'github')).toBe(false);
  });

  it('returns false for an empty integrations list', () => {
    expect(resolveSupportsConfiguration([], 'github')).toBe(false);
  });

  it('handles a mixed list of strings and objects, matching whichever is correct', () => {
    const integrations: readonly (string | ConfigurationWire)[] = ['integration_jira', { type: 'integration_github' }];
    expect(resolveSupportsConfiguration(integrations, 'github')).toBe(true);
  });
});

describe('resolveDisabledConfigFields', () => {
  const supportsConfig = true;

  it('is false while not editing (creating), even with a supported type and no title', () => {
    const configuration: ToolkitConfigurationState = {};
    expect(resolveDisabledConfigFields(configuration, false, supportsConfig)).toBe(false);
  });

  it('is false while editing but the configuration is in CreatePersonal mode', () => {
    const configuration: ToolkitConfigurationState = { elitea_title: ConfigurationMode.CreatePersonal };
    expect(resolveDisabledConfigFields(configuration, true, supportsConfig)).toBe(false);
  });

  it('is false while editing but the configuration is in CreateProject mode', () => {
    const configuration: ToolkitConfigurationState = { elitea_title: ConfigurationMode.CreateProject };
    expect(resolveDisabledConfigFields(configuration, true, supportsConfig)).toBe(false);
  });

  it('is true while editing a saved toolkit with no configuration title yet, for a type that supports configuration', () => {
    const configuration: ToolkitConfigurationState = {};
    expect(resolveDisabledConfigFields(configuration, true, supportsConfig)).toBe(true);
  });

  it('is false while editing a saved toolkit with no configuration title, for a type that does NOT support configuration', () => {
    const configuration: ToolkitConfigurationState = {};
    expect(resolveDisabledConfigFields(configuration, true, false)).toBe(false);
  });

  it('is false while editing a saved toolkit that already has a configuration title', () => {
    const configuration: ToolkitConfigurationState = { elitea_title: 'My Config' };
    expect(resolveDisabledConfigFields(configuration, true, supportsConfig)).toBe(false);
  });

  it('treats an undefined elitea_title as an empty string (falsy) for the disabled check', () => {
    const configuration: ToolkitConfigurationState = { elitea_title: undefined };
    expect(resolveDisabledConfigFields(configuration, true, supportsConfig)).toBe(true);
  });
});

describe('resolveIsLoading', () => {
  it('is true when fetching and the toolkit schemas have never loaded', () => {
    expect(resolveIsLoading(true, false, false)).toBe(true);
  });

  it('is false when fetching but the toolkit schemas have already loaded once (breaks the remount-loop)', () => {
    expect(resolveIsLoading(true, true, false)).toBe(false);
  });

  it('is false when not fetching and configurations are not loading', () => {
    expect(resolveIsLoading(false, true, false)).toBe(false);
  });

  it('is true when configurations are loading, regardless of the fetching/schema state', () => {
    expect(resolveIsLoading(false, true, true)).toBe(true);
  });

  it('coerces an undefined isLoadingConfigurations to false', () => {
    expect(resolveIsLoading(false, true, undefined)).toBe(false);
  });

  it('is true when both fetching-without-schemas and configurations-loading are true', () => {
    expect(resolveIsLoading(true, false, true)).toBe(true);
  });
});

describe('resolveOutOfBandFieldSync', () => {
  it('returns an empty list when editToolDetail has no settings or mcp_options', () => {
    expect(resolveOutOfBandFieldSync({}, {})).toEqual([]);
  });

  it('reports a settings field whose value differs from the form value', () => {
    const editToolDetail: ToolkitFormEditDetail = { settings: { url: 'new-value' } };
    const formValues = { settings: { url: 'old-value' } };
    expect(resolveOutOfBandFieldSync(editToolDetail, formValues)).toEqual([{ field: 'settings.url', value: 'new-value' }]);
  });

  it('does not report a settings field whose value is unchanged', () => {
    const editToolDetail: ToolkitFormEditDetail = { settings: { url: 'same' } };
    const formValues = { settings: { url: 'same' } };
    expect(resolveOutOfBandFieldSync(editToolDetail, formValues)).toEqual([]);
  });

  it('compares settings values by deep (JSON) equality, not reference', () => {
    const editToolDetail: ToolkitFormEditDetail = { settings: { nested: { a: 1 } } };
    const formValues = { settings: { nested: { a: 1 } } };
    expect(resolveOutOfBandFieldSync(editToolDetail, formValues)).toEqual([]);
  });

  it('reports an mcp_options field whose value differs from the form value', () => {
    const editToolDetail: ToolkitFormEditDetail = { meta: { mcp_options: { scope: 'new' } } };
    const formValues = { meta: { mcp_options: { scope: 'old' } } };
    expect(resolveOutOfBandFieldSync(editToolDetail, formValues)).toEqual([{ field: 'meta.mcp_options.scope', value: 'new' }]);
  });

  it('reports both settings and mcp_options diffs together', () => {
    const editToolDetail: ToolkitFormEditDetail = { settings: { url: 'new-url' }, meta: { mcp_options: { scope: 'new-scope' } } };
    const formValues = { settings: { url: 'old-url' }, meta: { mcp_options: { scope: 'old-scope' } } };
    expect(resolveOutOfBandFieldSync(editToolDetail, formValues)).toEqual([
      { field: 'settings.url', value: 'new-url' },
      { field: 'meta.mcp_options.scope', value: 'new-scope' },
    ]);
  });

  it('treats a missing form settings object as an implicit undefined for every key (all reported as changed)', () => {
    const editToolDetail: ToolkitFormEditDetail = { settings: { url: 'value' } };
    expect(resolveOutOfBandFieldSync(editToolDetail, {})).toEqual([{ field: 'settings.url', value: 'value' }]);
  });

  it('treats a missing form meta as an implicit undefined for every mcp_options key (all reported as changed)', () => {
    const editToolDetail: ToolkitFormEditDetail = { meta: { mcp_options: { scope: 'value' } } };
    expect(resolveOutOfBandFieldSync(editToolDetail, {})).toEqual([{ field: 'meta.mcp_options.scope', value: 'value' }]);
  });
});

describe('resolveCredentialReverts', () => {
  it('returns an empty list when there are no credential-like settings fields', () => {
    expect(resolveCredentialReverts({ url: 'plain-string' }, {})).toEqual([]);
  });

  it('ignores a settings field that is not credential-like (no elitea_title key)', () => {
    const current = { url: { host: 'x' } };
    expect(resolveCredentialReverts(current, {})).toEqual([]);
  });

  it('reverts a credential-like field whose private flag changed from its initial value', () => {
    const current = { api_key: { elitea_title: 'Shared Cred', private: true } };
    const initial = { api_key: { elitea_title: 'Shared Cred', private: false } };
    expect(resolveCredentialReverts(current, initial)).toEqual([{ field: 'settings.api_key', value: initial.api_key }]);
  });

  it('reverts a credential-like field whose elitea_title changed from its initial value', () => {
    const current = { api_key: { elitea_title: 'Changed Title', private: false } };
    const initial = { api_key: { elitea_title: 'Original Title', private: false } };
    expect(resolveCredentialReverts(current, initial)).toEqual([{ field: 'settings.api_key', value: initial.api_key }]);
  });

  it('does not revert a credential-like field whose private/elitea_title are unchanged from initial', () => {
    const current = { api_key: { elitea_title: 'Same', private: true } };
    const initial = { api_key: { elitea_title: 'Same', private: true } };
    expect(resolveCredentialReverts(current, initial)).toEqual([]);
  });

  it('reads the initial value defensively via ?. when there is no matching initial field at all', () => {
    const current = { api_key: { elitea_title: 'New', private: false } };
    expect(resolveCredentialReverts(current, {})).toEqual([{ field: 'settings.api_key', value: undefined }]);
  });

  it('reverts multiple credential-like fields independently', () => {
    const current = {
      api_key: { elitea_title: 'Changed', private: false },
      other_key: { elitea_title: 'Stable', private: true },
    };
    const initial = {
      api_key: { elitea_title: 'Original', private: false },
      other_key: { elitea_title: 'Stable', private: true },
    };
    expect(resolveCredentialReverts(current, initial)).toEqual([{ field: 'settings.api_key', value: initial.api_key }]);
  });
});

describe('applyAutoSelectFormReset', () => {
  /**
   * [R1 regression] The exact helper `editField` (`ToolkitForm.core.hooks.ts`)
   * calls unconditionally on every field change — pre-fix, this function
   * (and the whole call site) did not exist at all, so `onResetForm` was
   * never invoked under any condition. Confirmed failing pre-fix by
   * reverting `ToolkitForm.core.hooks.ts`'s call site locally and
   * re-running the higher-level `useToolkitFormCore` suite
   * (`ToolkitForm.core.hooks.test.tsx`), which exercises this same code
   * path end to end.
   */
  it('calls onResetForm with the auto-selected value merged into formValues when isAutoSelect is true', () => {
    const onResetForm = vi.fn();
    applyAutoSelectFormReset({ isAutoSelect: true }, { type: 'github', settings: { embedding_model: 'old' } }, 'settings.embedding_model', 'new', onResetForm);

    expect(onResetForm).toHaveBeenCalledWith({ type: 'github', settings: { embedding_model: 'new' } });
  });

  it('does not call onResetForm when isAutoSelect is false', () => {
    const onResetForm = vi.fn();
    applyAutoSelectFormReset({ isAutoSelect: false }, {}, 'name', 'x', onResetForm);
    expect(onResetForm).not.toHaveBeenCalled();
  });

  it('does not call onResetForm when options is undefined (a normal user edit)', () => {
    const onResetForm = vi.fn();
    applyAutoSelectFormReset(undefined, {}, 'name', 'x', onResetForm);
    expect(onResetForm).not.toHaveBeenCalled();
  });

  it('does not throw when onResetForm is undefined, even with isAutoSelect: true', () => {
    expect(() => applyAutoSelectFormReset({ isAutoSelect: true }, {}, 'name', 'x', undefined)).not.toThrow();
  });

  /**
   * [EL-6180 regression] `CredentialsSelect` auto-selects a sole saved
   * credential. Rebaselining on that pick leaves the form not dirty, so Save
   * stays disabled and the user cannot save the credential the app chose for
   * them. Baseline carve-out: `EliteaUI@a693e4fe`, `ToolkitForm.jsx:295`.
   * Fails against the pre-fix helper, which rebaselined for any isAutoSelect.
   */
  it('does not call onResetForm when the auto-selected field is in the credentials section', () => {
    const onResetForm = vi.fn();
    applyAutoSelectFormReset(
      { isAutoSelect: true, section: 'credentials' },
      { type: 'github', settings: { github_configuration: null } },
      'settings.github_configuration',
      { id: 7 },
      onResetForm,
    );
    expect(onResetForm).not.toHaveBeenCalled();
  });

  /** The carve-out is for `credentials` only: every other section keeps the suppression. */
  it('still calls onResetForm for an auto-selected field in another section', () => {
    const onResetForm = vi.fn();
    applyAutoSelectFormReset(
      { isAutoSelect: true, section: 'embedding' },
      { type: 'github', settings: { embedding_model: null } },
      'settings.embedding_model',
      { id: 9 },
      onResetForm,
    );
    expect(onResetForm).toHaveBeenCalledWith({ type: 'github', settings: { embedding_model: { id: 9 } } });
  });
});
