import { describe, expect, it } from 'vitest';

import { collectGraphAdmissionIssues } from './graphAdmission.helpers';
import type { YamlPipelineDocument, YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import {
  declaresNodeErrors,
  graphNodeSettingsIssues,
  NodeSettingsRefusalTexts,
  nodeSettingsFamilyKeys,
  readNodeSettings,
  writeNodeSettings,
} from './nodeSettings.helpers';

const node = (settings?: Record<string, unknown>): YamlPipelineNode =>
  ({ id: 'a', type: 'state_modifier', template: 'x', output: ['out'], transition: 'b', ...(settings ? { settings } : {}) });

const document = (settings?: Record<string, unknown>): YamlPipelineDocument => ({
  state: { out: 'str', count: 'str' },
  entry_point: 'a',
  nodes: [node(settings), { id: 'b', type: 'state_modifier', template: '{{ node_errors | length }}', input: ['node_errors'], output: ['count'], transition: 'END' }],
});

describe('node settings helpers', () => {
  it('writes no block for defaults and keeps the error route only with error output', () => {
    expect(writeNodeSettings(undefined, { on_error: 'stop' })).toBeUndefined();
    expect(writeNodeSettings({ execute_once: true }, { execute_once: false })).toBeUndefined();
    expect(writeNodeSettings({ on_error: 'error_output', error_transition: 'b' }, { on_error: 'continue' })).toEqual({ on_error: 'continue' });
    expect(writeNodeSettings(undefined, { always_output_data: true })).toEqual({ always_output_data: true });
    // Turning off retry_on_fail drops coupled max_tries and wait_between_tries_ms cleanly.
    expect(
      writeNodeSettings({ retry_on_fail: true, max_tries: 4, wait_between_tries_ms: 500 }, { retry_on_fail: false }),
    ).toBeUndefined();
    expect(
      writeNodeSettings({ retry_on_fail: true, max_tries: 4, timeout_ms: 5000 }, { retry_on_fail: false }),
    ).toEqual({ timeout_ms: 5000 });
    expect(writeNodeSettings({ timeout_ms: 5000 }, { timeout_ms: undefined })).toBeUndefined();
  });

  it('reads defaults for an absent or invalid block', () => {
    expect(readNodeSettings(node()).on_error).toBe('stop');
    expect(readNodeSettings(node({ bogus: 1 })).execute_once).toBe(false);
    expect(readNodeSettings(node({ on_error: 'continue', execute_once: true }))).toMatchObject({ on_error: 'continue', execute_once: true });
  });

  it('declares node_errors only when a node continues on error (D2)', () => {
    expect(declaresNodeErrors(document({ on_error: 'continue' }))).toBe(true);
    expect(declaresNodeErrors(document({ execute_once: true }))).toBe(false);
    // A node may read the declared list; without a continuing node it is undeclared.
    const subjects = (doc: YamlPipelineDocument) => collectGraphAdmissionIssues(doc).filter((issue) => issue.severity !== 'warning').map((issue) => issue.subject);
    expect(subjects(document({ on_error: 'continue' }))).toEqual([]);
    expect(subjects(document({ execute_once: true }))).toContain('node_errors');
  });

  it('refuses settings.error_transition targeting a dedicated failure_handler node (SEC-5B1B-03)', () => {
    const doc: YamlPipelineDocument = {
      state: { out: 'str', err_ctx: 'dict', handled: 'str' },
      entry_point: 'a',
      nodes: [
        {
          id: 'a',
          type: 'state_modifier',
          template: 'x',
          output: ['out'],
          settings: { on_error: 'error_output', error_transition: 'dedicated' },
          transition: 'producer',
        },
        {
          id: 'producer',
          type: 'code',
          language: 'python',
          code: { type: 'fixed', value: 'return 1' },
          input: [],
          output: ['out'],
          recovery: {
            max_attempts: 1,
            on_failure: { route: 'dedicated', error_input: 'err_ctx', classes: ['invalid_result'] },
          },
          transition: 'END',
        },
        {
          id: 'dedicated',
          type: 'code',
          language: 'python',
          code: { type: 'fixed', value: 'return "ok"' },
          input: ['err_ctx'],
          output: ['handled'],
          failure_handler: { error_input: 'err_ctx' },
          transition: 'END',
        },
      ],
    };
    const issues = graphNodeSettingsIssues(doc);
    expect(issues).toHaveLength(1);
    expect(issues[0]?.message).toBe(`Node a: ${NodeSettingsRefusalTexts['error_transition_target']}`);
  });

  it('guards NodeSettingsFamilies lookup against prototype properties (SEC-5B1B-04)', () => {
    for (const protoKey of ['constructor', 'toString', '__proto__', 'valueOf']) {
      expect(nodeSettingsFamilyKeys(protoKey)).toBeUndefined();
      const doc: YamlPipelineDocument = {
        state: { out: 'str' },
        entry_point: 'a',
        nodes: [{ id: 'a', type: protoKey, settings: { execute_once: true }, transition: 'END' }],
      };
      const issues = graphNodeSettingsIssues(doc);
      expect(issues).toHaveLength(1);
      expect(issues[0]?.message).toBe(`Node a: ${NodeSettingsRefusalTexts['family_not_yet']}`);
    }
  });

  it('admits settings on http nodes and enforces the 30s HTTP timeout ceiling', () => {
    expect(nodeSettingsFamilyKeys('http')).toContain('retry_on_fail');
    expect(nodeSettingsFamilyKeys('http')).toContain('timeout_ms');
    const okDoc: YamlPipelineDocument = {
      state: { fx: 'dict' },
      entry_point: 'fetch',
      nodes: [{
        id: 'fetch',
        type: 'http',
        revision: 2,
        operation: 'fx_reference.rates',
        request: {},
        response: { mode: 'json' },
        settings: { retry_on_fail: true, max_tries: 3, timeout_ms: 30_000 },
        output: ['fx'],
        transition: 'END',
      }],
    };
    expect(graphNodeSettingsIssues(okDoc)).toEqual([]);
    const overDoc: YamlPipelineDocument = {
      ...okDoc,
      nodes: [{
        ...(okDoc.nodes?.[0] as YamlPipelineNode),
        settings: { timeout_ms: 30_001 },
      }],
    };
    const issues = graphNodeSettingsIssues(overDoc);
    expect(issues).toHaveLength(1);
    expect(issues[0]?.message).toBe(`Node fetch: ${NodeSettingsRefusalTexts['http_timeout_range']}`);
  });
});
