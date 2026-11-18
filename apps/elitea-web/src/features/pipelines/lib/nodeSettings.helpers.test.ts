import { describe, expect, it } from 'vitest';

import { collectGraphAdmissionIssues } from './graphAdmission.helpers';
import type { YamlPipelineDocument, YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import { declaresNodeErrors, readNodeSettings, writeNodeSettings } from './nodeSettings.helpers';

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
});
