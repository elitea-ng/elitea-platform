import { describe, expect, it } from 'vitest';

import { DEFAULT_MAX_TOKENS, DEFAULT_TEMPERATURE, toAiAssistantLlmSettings, toPipelineToolEntries } from './flowEditorVersionInputs.helpers';

/** `noUncheckedIndexedAccess` types array element access (and destructuring) as possibly `undefined` -- these tests always feed exactly one input tool, so a thrown assertion here would itself be the real, informative test failure. */
function only<T>(entries: readonly T[]): T {
  const [entry] = entries;
  if (entry === undefined) throw new Error('expected exactly one entry');
  return entry;
}

describe('toPipelineToolEntries', () => {
  it('returns an empty array for undefined/empty input', () => {
    expect(toPipelineToolEntries(undefined)).toEqual([]);
    expect(toPipelineToolEntries([])).toEqual([]);
  });

  it('maps a VersionToolRef-shaped entry using its top-level fields (numeric id -> string)', () => {
    const entry = only(toPipelineToolEntries([{ id: 42, type: 'toolkit', name: 'search', entity_type: 'toolkit', selected_tools: ['a', 'b'] }]));
    expect(entry).toEqual({
      id: '42',
      type: 'toolkit',
      name: 'search',
      agent_type: 'toolkit',
      settings: { selected_tools: ['a', 'b'] },
    });
  });

  it('reads toolkit_name/description/agent_type/meta.mcp/tools off the opaque config blob when absent at the top level', () => {
    const entry = only(toPipelineToolEntries([
      {
        id: 1,
        config: {
          toolkit_name: 'GitHub',
          description: 'GitHub toolkit',
          agent_type: 'pipeline',
          meta: { mcp: true },
          tools: ['create_issue', { name: 'list_prs' }],
        },
      },
    ]));
    expect(entry.toolkit_name).toBe('GitHub');
    expect(entry.description).toBe('GitHub toolkit');
    expect(entry.agent_type).toBe('pipeline');
    expect(entry.meta).toEqual({ mcp: true });
    expect(entry.tools).toEqual(['create_issue', { name: 'list_prs' }]);
  });

  it('falls back to the settings blob when config is absent, and prefers top-level fields over either blob', () => {
    const entry = only(toPipelineToolEntries([
      {
        id: 2,
        toolkit_name: 'top-level-name',
        settings: { toolkit_name: 'blob-name', selected_tools: ['x'] },
      },
    ]));
    expect(entry.toolkit_name).toBe('top-level-name');
    expect(entry.settings).toEqual({ selected_tools: ['x'] });
  });

  it('falls back to entity_type for agent_type when neither blob nor top-level agent_type is present', () => {
    const entry = only(toPipelineToolEntries([{ id: 3, entity_type: 'application' }]));
    expect(entry.agent_type).toBe('application');
  });

  it('omits fields entirely rather than setting them to undefined when no source data is found', () => {
    const entry = only(toPipelineToolEntries([{ type: 'toolkit' }]));
    expect(entry).toEqual({ type: 'toolkit' });
    expect(Object.keys(entry)).toEqual(['type']);
  });

  it('ignores non-object entries and non-string/non-array garbage in the blob without throwing', () => {
    const entries = toPipelineToolEntries([null, 'not-an-object', 42, { id: 5, config: { toolkit_name: 123, tools: 'not-an-array', meta: 'not-an-object' } }]);
    expect(entries).toHaveLength(4);
    expect(entries[3]).toEqual({ id: '5' });
  });

  it('falls back to the toolkit\'s own selected_tools when the mapping selection is the empty default (#2003)', () => {
    // `entity_tool_mapping.selected_tools` defaults to `[]`; the runtime treats that as "no narrowing".
    const entry = only(toPipelineToolEntries([{ id: 7, type: 'github', name: 'gh', selected_tools: [], config: { selected_tools: ['create_issue', 'list_issues'] } }]));
    expect(entry.settings).toEqual({ selected_tools: ['create_issue', 'list_issues'] });
  });

  it('lets a non-empty mapping selection narrow the toolkit\'s own list', () => {
    const entry = only(toPipelineToolEntries([{ id: 7, type: 'github', selected_tools: ['list_issues'], config: { selected_tools: ['create_issue', 'list_issues'] } }]));
    expect(entry.settings).toEqual({ selected_tools: ['list_issues'] });
  });

  it('filters non-string entries out of selected_tools/tools arrays', () => {
    const entry = only(toPipelineToolEntries([{ id: 6, selected_tools: ['a', 42, null, 'b'], config: { tools: ['x', 7, { name: 'y' }] } }]));
    expect(entry.settings).toEqual({ selected_tools: ['a', 'b'] });
    expect(entry.tools).toEqual(['x', { name: 'y' }]);
  });
});

describe('toAiAssistantLlmSettings', () => {
  it('defaults model_name to empty string and applies DEFAULT_TEMPERATURE/DEFAULT_MAX_TOKENS when llm_settings is undefined', () => {
    expect(toAiAssistantLlmSettings(undefined)).toEqual({
      model_name: '',
      temperature: DEFAULT_TEMPERATURE,
      max_tokens: DEFAULT_MAX_TOKENS,
    });
  });

  it('defaults individual missing fields while keeping the ones that are present', () => {
    expect(toAiAssistantLlmSettings({ model_name: 'gpt-4o' })).toEqual({
      model_name: 'gpt-4o',
      temperature: DEFAULT_TEMPERATURE,
      max_tokens: DEFAULT_MAX_TOKENS,
    });
  });

  it('passes through real, fully-populated wire values unchanged', () => {
    expect(toAiAssistantLlmSettings({ model_name: 'gpt-4o', temperature: 0.9, max_tokens: 2048 })).toEqual({
      model_name: 'gpt-4o',
      temperature: 0.9,
      max_tokens: 2048,
    });
  });

  it('never produces an integration_uid key (no wire shape in this app carries one)', () => {
    const result = toAiAssistantLlmSettings({ model_name: 'gpt-4o' });
    expect('integration_uid' in result).toBe(false);
  });
});
