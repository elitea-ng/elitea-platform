import { describe, expect, it } from 'vitest';

import {
  getDefaultInputMappingOfTool,
  getEnumList,
  getInputMappingDefaultValue,
  getRequiredInputsAndTooltips,
} from './flowEditorInputMapping.helpers';

describe('getInputMappingDefaultValue', () => {
  it('returns the first enum value for a non-array type with an enum', () => {
    expect(getInputMappingDefaultValue(['a', 'b'], 'string', {}, 'key')).toBe('a');
  });

  it('returns [] for an array type even with an enum', () => {
    expect(getInputMappingDefaultValue(['a', 'b'], 'array', {}, 'key')).toEqual([]);
  });

  it('falls back to defaultValues[key], else empty string', () => {
    expect(getInputMappingDefaultValue(undefined, 'string', { key: 'preset' }, 'key')).toBe('preset');
    expect(getInputMappingDefaultValue(undefined, 'string', {}, 'key')).toBe('');
  });
});

describe('getEnumList', () => {
  it('fixed: returns the schema enum as-is', () => {
    expect(getEnumList('fixed', ['a', 'b'], [])).toEqual(['a', 'b']);
  });

  it('variable: maps input options to their values', () => {
    expect(getEnumList('variable', undefined, [{ value: 'x' }, { value: 'y' }])).toEqual(['x', 'y']);
  });

  it('anything else: empty array', () => {
    expect(getEnumList('other', ['a'], [])).toEqual([]);
  });
});

describe('getDefaultInputMappingOfTool', () => {
  it('builds a mapping from a tool schema, defaulting values by JSON-schema type', () => {
    const toolkitSchemas = {
      github: {
        properties: {
          selected_tools: {
            args_schemas: {
              create_issue: {
                properties: {
                  title: { type: 'string' },
                  count: { type: 'integer', default: 1 },
                  flag: { type: 'boolean' },
                },
              },
            },
          },
        },
      },
    };
    const result = getDefaultInputMappingOfTool(toolkitSchemas, 'create_issue', undefined, { type: 'github' });
    expect(result.mapping).toStrictEqual({
      title: { type: 'fixed', value: '' },
      count: { type: 'fixed', value: 1 },
      flag: { type: 'fixed', value: false },
    });
    expect(result.defaultValues).toEqual({ title: '', count: 1, flag: false });
  });

  it('writes no undefined-valued key: an enum appears only when the schema declares one, and a stale one is dropped', () => {
    const toolkitSchemas = {
      artifact: {
        properties: {
          selected_tools: {
            args_schemas: {
              list_files: {
                properties: {
                  bucket_name: { anyOf: [{ type: 'string' }, { type: 'null' }], default: null },
                  order: { type: 'string', enum: ['asc', 'desc'] },
                  folder: { type: 'string' },
                },
              },
            },
          },
        },
      },
    };
    const existingMapping = { folder: { type: 'fixed' as const, value: 'docs', enum: ['stale'] } };
    const result = getDefaultInputMappingOfTool(toolkitSchemas, 'list_files', existingMapping, { type: 'artifact' });
    expect(result.mapping).toStrictEqual({
      bucket_name: { type: 'fixed', value: null },
      order: { type: 'fixed', value: 'asc', enum: ['asc', 'desc'] },
      folder: { type: 'fixed', value: 'docs' },
    });
    expect(existingMapping.folder.enum).toEqual(['stale']);
  });

  it('preserves an existing mapping entry rather than resetting it', () => {
    const toolkitSchemas = {
      github: { properties: { selected_tools: { args_schemas: { create_issue: { properties: { title: { type: 'string' } } } } } } },
    };
    const existingMapping = { title: { type: 'variable' as const, value: 'some_var' } };
    const result = getDefaultInputMappingOfTool(toolkitSchemas, 'create_issue', existingMapping, { type: 'github' });
    expect(result.mapping['title']).toMatchObject({ type: 'variable', value: 'some_var' });
  });

  it('special-cases the "application" toolkit type into a task + agent-variables mapping', () => {
    const result = getDefaultInputMappingOfTool(undefined, undefined, undefined, {
      type: 'application',
      variables: [{ name: 'topic', value: 'default topic' }],
    });
    expect(result.mapping).toMatchObject({
      task: { type: 'fstring', value: '' },
      topic: { type: 'fixed', value: 'default topic' },
    });
    expect(result.mappingInfo?.['task']).toMatchObject({ type: 'fstring' });
  });

  it('retains saved child variable sources and JSON values when defaults are derived again', () => {
    const existingMapping = {
      task: { type: 'fstring', value: 'Run {topic}' },
      count: { type: 'variable', value: 'parent_count' },
      label: { type: 'fstring', value: 'for {topic}', multiline: true },
      items: { type: 'variable', value: 'parent_items' },
      enabled: { type: 'fixed', value: false },
      attempts: { type: 'fixed', value: 0 },
      options: { type: 'fixed', value: { mode: 'explicit', tags: ['one'] } },
      empty_note: { type: 'fixed', value: '' },
      removed: { type: 'fixed', value: 'old child value' },
    };
    const toolkit = {
      type: 'application',
      variables: [
        { name: 'count', value: 1 },
        { name: 'label', value: 'default label' },
        { name: 'items', value: [] },
        { name: 'enabled', value: true },
        { name: 'attempts', value: 3 },
        { name: 'options', value: { mode: 'default' } },
        { name: 'empty_note', value: 'default note' },
        { name: 'fresh', value: ['new default'] },
      ],
    };
    const expectedMapping = {
      task: existingMapping.task,
      count: existingMapping.count,
      label: existingMapping.label,
      items: existingMapping.items,
      enabled: existingMapping.enabled,
      attempts: existingMapping.attempts,
      options: existingMapping.options,
      empty_note: existingMapping.empty_note,
      fresh: { type: 'fixed', value: ['new default'] },
    };

    const result = getDefaultInputMappingOfTool(undefined, undefined, existingMapping, toolkit);

    expect(result.mapping).toEqual(expectedMapping);
    for (const variable of toolkit.variables) {
      expect(result.mappingInfo?.[variable.name]).toEqual({
        tooltip: 'This is a variable from the agent',
        ...expectedMapping[variable.name as keyof typeof expectedMapping],
      });
    }
    expect(result.mapping).not.toHaveProperty('removed');
    expect(result.mappingInfo).not.toHaveProperty('removed');
    expect(existingMapping.removed).toEqual({ type: 'fixed', value: 'old child value' });
    expect(getDefaultInputMappingOfTool(undefined, undefined, result.mapping as typeof expectedMapping, toolkit)).toEqual(result);
  });

  it('keeps an unsupported saved source available for validation instead of replacing it with a valid default', () => {
    const existingMapping = { topic: { type: 'unsupported', value: 'invalid source' } };
    const result = getDefaultInputMappingOfTool(undefined, undefined, existingMapping, {
      type: 'application',
      variables: [{ name: 'topic', value: 'valid default' }],
    });

    expect(result.mapping['topic']).toEqual(existingMapping.topic);
    expect(result.mappingInfo?.['topic']).toMatchObject(existingMapping.topic);
  });

  it('returns the existing mapping unchanged when the tool schema cannot be resolved yet', () => {
    const result = getDefaultInputMappingOfTool(undefined, 'some_tool', { a: { type: 'fixed', value: 1 } }, { type: 'custom' });
    expect(result.mapping).toEqual({ a: { type: 'fixed', value: 1 } });
    expect(result.defaultValues).toEqual({});
  });

  it('extracts args_schema from available_mcp_tools for a remote MCP tool by value or label', () => {
    const toolkit = {
      type: 'mcp',
      meta: { mcp: true },
      settings: {
        available_mcp_tools: [
          { value: 'search', label: 'Search', args_schema: { properties: { query: { type: 'string' } } } },
        ],
      },
    };
    const result = getDefaultInputMappingOfTool(undefined, 'search', undefined, toolkit);
    expect(result.mapping).toHaveProperty('query');
  });
});

describe('getRequiredInputsAndTooltips', () => {
  it('special-cases "application" toolkit to require just `task`', () => {
    const result = getRequiredInputsAndTooltips(undefined, undefined, {
      type: 'application',
      settings: { variables: [{ name: 'topic' }] },
    });
    expect(result.required).toEqual(['task']);
    expect(result.tooltips?.['task']).toBeTypeOf('string');
    expect(result.tooltips?.['topic']).toBeTypeOf('string');
  });

  it('reads `required` off the resolved tool schema', () => {
    const toolkitTypes = {
      github: { properties: { selected_tools: { args_schemas: { create_issue: { required: ['title'] } } } } },
    };
    const result = getRequiredInputsAndTooltips(toolkitTypes, 'create_issue', { type: 'github' });
    expect(result.required).toEqual(['title']);
  });

  it('falls back to inputSchema.required for an MCP tool, else []', () => {
    const toolkit = {
      type: 'mcp',
      meta: { mcp: true },
      settings: { available_mcp_tools: [{ value: 'search', args_schema: { inputSchema: { required: ['query'] } } }] },
    };
    expect(getRequiredInputsAndTooltips(undefined, 'search', toolkit).required).toEqual(['query']);
    expect(getRequiredInputsAndTooltips(undefined, 'missing', { type: 'github' }).required).toEqual([]);
  });
});
