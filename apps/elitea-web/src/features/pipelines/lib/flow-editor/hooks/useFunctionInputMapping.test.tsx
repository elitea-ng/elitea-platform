import { act, waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithRouterAndProject } from '../../../__tests__/testUtils';
import type { YamlPipelineDocument } from '../helpers/pipelineFlow.types';
import type { UseFunctionInputMappingArgs, UseFunctionInputMappingResult, VersionTool } from './useFunctionInputMapping';
import { useFunctionInputMapping } from './useFunctionInputMapping';

const BASE = '/api/v2';
const PROJECT_ID = 'proj-1';

/** Renders the hook via a real component tree (`useSelectedProjectId`/`useToolkitTypeSchemas` need router + query-client context) and hands every render's result out through `onResult`. */
function HookProbe({ onResult, ...args }: UseFunctionInputMappingArgs & { onResult: (result: UseFunctionInputMappingResult) => void }) {
  onResult(useFunctionInputMapping(args));
  return null;
}

describe('useFunctionInputMapping', () => {
  beforeEach(() => {
    configureGeneratedClient({ baseUrl: BASE });
    // Empty catalogue: no entry for the `custom` toolkit type -- reproduces the
    // disclosed "dynamic schema fetch missing" gap (file header, item 5) for a
    // non-MCP toolkit, since `properties` resolves to `{}` either way.
    server.use(http.get(`${BASE}/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`, () => HttpResponse.json({})));
    // The catalogue read (#440) runs for every toolkit without an explicit
    // `selected_tools` list, which is every fixture below. A per-test
    // `server.use` still wins: msw puts run-time handlers ahead of this one.
    server.use(http.post(`${BASE}/elitea_core/toolkit_discover_tools/prompt_lib/${PROJECT_ID}/:toolkitType`, () => HttpResponse.json({ tools: [], total: 0 })));
  });

  afterEach(() => {
    resetGeneratedClient();
  });

  it('preserves an already-saved input_mapping for a non-MCP toolkit when the dynamic schema fetch is unavailable, instead of wiping it to {}', async () => {
    const existingInputMapping = { existingField: { type: 'fixed', value: 'kept' } };
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'Node1', tool: 'my_tool', toolkit_name: 'custom_toolkit', input_mapping: existingInputMapping }],
    };
    const setYamlJsonObject = vi.fn();
    const versionTools = [{ type: 'custom', name: 'custom_toolkit', toolkit_name: 'custom_toolkit' }];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="Node1"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.selectedToolkit).toBeDefined());
    await waitFor(() => expect(latest?.inputMappings).toEqual(existingInputMapping));

    // The hook must never have persisted an empty (wiped) input_mapping for this node.
    for (const call of setYamlJsonObject.mock.calls) {
      const persistedNode = (call[0] as YamlPipelineDocument).nodes?.find(node => node.id === 'Node1');
      if (persistedNode?.input_mapping) {
        expect(persistedNode.input_mapping).toEqual(existingInputMapping);
      }
    }
  });

  it('resolves an MCP toolkit tool schema via available_mcp_tools and writes its required-field default mapping', async () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'McpNode', tool: 'my_mcp_tool', toolkit_name: 'my_mcp_toolkit' }],
    };
    const setYamlJsonObject = vi.fn();
    const versionTools = [
      {
        type: 'mcp',
        name: 'my_mcp_toolkit',
        toolkit_name: 'my_mcp_toolkit',
        settings: {
          available_mcp_tools: [
            {
              value: 'my_mcp_tool',
              args_schema: { properties: { foo: { type: 'string', description: 'Foo field' } }, required: ['foo'] },
            },
          ],
        },
      },
    ];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="McpNode"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.selectedToolkit).toBeDefined());
    await waitFor(() => expect(latest?.requiredInputs).toEqual(['foo']));
    await waitFor(() => expect(latest?.inputMappings['foo']).toBeDefined());

    const written = setYamlJsonObject.mock.calls.at(-1)?.[0] as YamlPipelineDocument;
    expect(written.nodes?.find(node => node.id === 'McpNode')?.input_mapping).toHaveProperty('foo');
  });

  it('onChangeTool(newValue) recomputes the default mapping for the new tool and writes { tool, input_mapping } together', async () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'Node1', toolkit_name: 'custom_toolkit' }],
    };
    const setYamlJsonObject = vi.fn();
    const versionTools = [{ type: 'custom', name: 'custom_toolkit', toolkit_name: 'custom_toolkit' }];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="Node1"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.selectedToolkit).toBeDefined());
    setYamlJsonObject.mockClear();

    act(() => {
      latest?.onChangeTool('new_tool');
    });

    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    const written = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    const writtenNode = written.nodes?.find(node => node.id === 'Node1');
    expect(writtenNode?.tool).toBe('new_tool');
    expect(writtenNode?.input_mapping).toEqual({});
  });

  it('onChangeTool(undefined) clears both tool and input_mapping on the node', async () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'Node1', tool: 'existing_tool', toolkit_name: 'custom_toolkit', input_mapping: { a: { type: 'fixed', value: 'x' } } }],
    };
    const setYamlJsonObject = vi.fn();
    const versionTools = [{ type: 'custom', name: 'custom_toolkit', toolkit_name: 'custom_toolkit' }];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="Node1"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.selectedToolkit).toBeDefined());
    setYamlJsonObject.mockClear();

    act(() => {
      latest?.onChangeTool(undefined);
    });

    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    const written = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    const writtenNode = written.nodes?.find(node => node.id === 'Node1');
    expect(writtenNode?.tool).toBeUndefined();
    expect(writtenNode?.input_mapping).toBeUndefined();
  });

  it('onChangeMapping writes a required-field value straight through updateYamlNodeInputMappingVariable', async () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'McpNode', tool: 'my_mcp_tool', toolkit_name: 'my_mcp_toolkit' }],
    };
    const setYamlJsonObject = vi.fn();
    const versionTools = [
      {
        type: 'mcp',
        name: 'my_mcp_toolkit',
        toolkit_name: 'my_mcp_toolkit',
        settings: {
          available_mcp_tools: [
            { value: 'my_mcp_tool', args_schema: { properties: { foo: { type: 'string' } }, required: ['foo'] } },
          ],
        },
      },
    ];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="McpNode"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.requiredInputs).toEqual(['foo']));
    setYamlJsonObject.mockClear();

    act(() => {
      latest?.onChangeMapping('foo', { type: 'fixed', value: 'bar' });
    });

    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    const written = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    expect(written.nodes?.find(node => node.id === 'McpNode')?.input_mapping).toMatchObject({
      foo: { type: 'fixed', value: 'bar' },
    });
  });

  it('onChangeMapping removes an optional field from input_mapping entirely when cleared to an empty value', async () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [
        {
          id: 'McpNode',
          tool: 'my_mcp_tool',
          toolkit_name: 'my_mcp_toolkit',
          input_mapping: { foo: { type: 'fixed', value: 'x' }, optionalField: { type: 'fixed', value: 'kept' } },
        },
      ],
    };
    const setYamlJsonObject = vi.fn();
    const versionTools = [
      {
        type: 'mcp',
        name: 'my_mcp_toolkit',
        toolkit_name: 'my_mcp_toolkit',
        settings: {
          available_mcp_tools: [
            {
              value: 'my_mcp_tool',
              args_schema: { properties: { foo: { type: 'string' }, optionalField: { type: 'string' } }, required: ['foo'] },
            },
          ],
        },
      },
    ];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="McpNode"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.requiredInputs).toEqual(['foo']));
    setYamlJsonObject.mockClear();

    // 'optionalField' is not in requiredInputs and the new value is empty -> deletion branch.
    act(() => {
      latest?.onChangeMapping('optionalField', { type: 'fixed', value: '' });
    });

    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    const written = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    const writtenMapping = written.nodes?.find(node => node.id === 'McpNode')?.input_mapping;
    expect(writtenMapping).not.toHaveProperty('optionalField');
    expect(writtenMapping).toHaveProperty('foo');
  });

  it('resolveToolkitIdentifier: a toolkit-bearing node type (agent/toolkit/mcp) with no explicit toolkit_name/tool still counts as an explicit reference (resolves to undefined, not the node id)', async () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'AgentNode', type: 'agent' }] };
    const setYamlJsonObject = vi.fn();

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="AgentNode"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={[]}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest).toBeDefined());
    expect(latest?.toolkit).toBeUndefined();
    expect(latest?.selectedToolkit).toBeUndefined();
  });

  it('resolveToolkitIdentifier: a plain node with no toolkit_name/tool and a non-toolkit-bearing type resolves the toolkit identifier to its own node id (also covers versionTools === undefined, not just [])', async () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'PlainNode' }] };
    const setYamlJsonObject = vi.fn();

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="PlainNode"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={undefined}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest).toBeDefined());
    expect(latest?.toolkit).toBe('PlainNode');
  });

  it('resolveToolkitIdentifier: falls back to the node\'s `tool` field as the toolkit identifier when toolkit_name is absent', async () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', tool: 'my_toolkit_via_tool_field' }] };
    const setYamlJsonObject = vi.fn();

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="Node1"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={[]}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest).toBeDefined());
    expect(latest?.toolkit).toBe('my_toolkit_via_tool_field');
  });

  it('selectedToolkit resolution: computes a missing toolkit_name via the schema helper (falls back to the cleaned tool name)', async () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', tool: 'RawName' }] };
    const setYamlJsonObject = vi.fn();
    // No `toolkit_name` field -> the map step must compute one via `getToolkitNameFromSchema`.
    const versionTools = [{ type: 'unknown_type', name: 'RawName' }];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="Node1"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.selectedToolkit).toBeDefined());
    expect(latest?.selectedToolkit?.name).toBe('RawName');
  });

  it('selectedToolkit resolution: matches by name when the entry\'s toolkit_name does not match the resolved toolkit identifier', async () => {
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', tool: 'my_toolkit' }] };
    const setYamlJsonObject = vi.fn();
    const versionTools = [{ type: 'custom', name: 'my_toolkit', toolkit_name: 'unrelated_id' }];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="Node1"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.selectedToolkit).toBeDefined());
    expect(latest?.selectedToolkit?.name).toBe('my_toolkit');
  });

  it('mcpArgsSchemas: skips an available_mcp_tools entry missing args_schema while still resolving the one that has it', async () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'McpNode', tool: 'my_mcp_tool', toolkit_name: 'my_mcp_toolkit' }],
    };
    const setYamlJsonObject = vi.fn();
    const versionTools = [
      {
        type: 'mcp',
        name: 'my_mcp_toolkit',
        toolkit_name: 'my_mcp_toolkit',
        settings: {
          available_mcp_tools: [
            { value: 'incomplete_tool' }, // no args_schema -- must not throw, must not be indexed
            { value: 'my_mcp_tool', args_schema: { properties: { foo: { type: 'string' } }, required: ['foo'] } },
          ],
        },
      },
    ];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="McpNode"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.requiredInputs).toEqual(['foo']));
    expect(latest?.inputMappings).toHaveProperty('foo');
  });

  it('treats a toolkit as MCP-like via its own top-level meta.mcp flag (not settings.meta.mcp), matching entities/toolkit\'s isMcpToolkit', async () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'MetaFlaggedNode', tool: 'my_mcp_tool', toolkit_name: 'my_meta_flagged_toolkit' }],
    };
    const setYamlJsonObject = vi.fn();
    const versionTools = [
      {
        // Deliberately NOT type: 'mcp' / 'mcp_*' -- only the top-level meta.mcp flag
        // identifies this toolkit as MCP-like, exercising the previously-broken read path.
        type: 'custom',
        name: 'my_meta_flagged_toolkit',
        toolkit_name: 'my_meta_flagged_toolkit',
        meta: { mcp: true },
        settings: {
          available_mcp_tools: [
            { value: 'my_mcp_tool', args_schema: { properties: { foo: { type: 'string' } }, required: ['foo'] } },
          ],
        },
      },
    ];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="MetaFlaggedNode"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.selectedToolkit).toBeDefined());
    // Required inputs must resolve from mcpArgsSchemas (available_mcp_tools), not the
    // always-empty dynamicArgsSchemas -- would be [] before the meta.mcp path fix.
    await waitFor(() => expect(latest?.requiredInputs).toEqual(['foo']));
    expect(latest?.isSchemaResolved).toBe(true);
    await waitFor(() => expect(latest?.inputMappings).toHaveProperty('foo'));

    const written = setYamlJsonObject.mock.calls.at(-1)?.[0] as YamlPipelineDocument;
    expect(written.nodes?.find(node => node.id === 'MetaFlaggedNode')?.input_mapping).toHaveProperty('foo');
  });

  it('isSchemaResolved is false for a non-MCP toolkit whose type has no static schema (the disclosed dynamic-fetch gap)', async () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'Node1', tool: 'my_tool', toolkit_name: 'custom_toolkit' }],
    };
    const setYamlJsonObject = vi.fn();
    const versionTools = [{ type: 'custom', name: 'custom_toolkit', toolkit_name: 'custom_toolkit' }];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="Node1"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.selectedToolkit).toBeDefined());
    expect(latest?.isSchemaResolved).toBe(false);
  });

  it('onChangeTool on an MCP-like toolkit resolves the new tool\'s schema via mcpArgsSchemas (not the always-empty dynamicArgsSchemas)', async () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'McpNode', tool: 'my_mcp_tool', toolkit_name: 'my_mcp_toolkit' }],
    };
    const setYamlJsonObject = vi.fn();
    const versionTools = [
      {
        type: 'mcp',
        name: 'my_mcp_toolkit',
        toolkit_name: 'my_mcp_toolkit',
        settings: {
          available_mcp_tools: [
            { value: 'my_mcp_tool', args_schema: { properties: { foo: { type: 'string' } }, required: ['foo'] } },
            { value: 'other_mcp_tool', args_schema: { properties: { bar: { type: 'string' } }, required: ['bar'] } },
          ],
        },
      },
    ];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="McpNode"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.requiredInputs).toEqual(['foo']));
    setYamlJsonObject.mockClear();

    act(() => {
      latest?.onChangeTool('other_mcp_tool');
    });

    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    const written = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    const writtenNode = written.nodes?.find(node => node.id === 'McpNode');
    expect(writtenNode?.tool).toBe('other_mcp_tool');
    expect(writtenNode?.input_mapping).toHaveProperty('bar');
  });

  it('onChangeMapping: with no explicit type and no prior mappingInfo entry, defaults the recorded type to "fixed"', async () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'McpNode', tool: 'my_mcp_tool', toolkit_name: 'my_mcp_toolkit' }],
    };
    const setYamlJsonObject = vi.fn();
    const versionTools = [
      {
        type: 'mcp',
        name: 'my_mcp_toolkit',
        toolkit_name: 'my_mcp_toolkit',
        settings: {
          available_mcp_tools: [
            { value: 'my_mcp_tool', args_schema: { properties: { foo: { type: 'string' } }, required: ['foo'] } },
          ],
        },
      },
    ];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="McpNode"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.requiredInputs).toEqual(['foo']));

    // 'freshVar' is not one of the schema's own properties, so mappingInfo has never seen it
    // before this call -- no `type` on the incoming value AND no prior entry -> falls all the
    // way through the `??` chain to the literal 'fixed' default.
    act(() => {
      latest?.onChangeMapping('freshVar', { value: 'first' });
    });
    await waitFor(() => expect((latest?.mappingInfo['freshVar'] as { type?: string })?.type).toBe('fixed'));

    // Second call for the SAME variable, still no explicit `type` -> this time a prior
    // mappingInfo entry exists, so the middle `??` operand (prev's own type) is what wins.
    act(() => {
      latest?.onChangeMapping('freshVar', { value: 'second' });
    });
    await waitFor(() => expect((latest?.mappingInfo['freshVar'] as { type?: string; value?: unknown })?.value).toBe('second'));
    expect((latest?.mappingInfo['freshVar'] as { type?: string })?.type).toBe('fixed');
  });

  it('onChangeMapping: clearing an optional field to empty that was never in the saved input_mapping still writes through updateYamlNodeInputMappingVariable (nothing to delete)', async () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'McpNode', tool: 'my_mcp_tool', toolkit_name: 'my_mcp_toolkit', input_mapping: { foo: { type: 'fixed', value: 'kept' } } }],
    };
    const setYamlJsonObject = vi.fn();
    const versionTools = [
      {
        type: 'mcp',
        name: 'my_mcp_toolkit',
        toolkit_name: 'my_mcp_toolkit',
        settings: {
          available_mcp_tools: [
            {
              value: 'my_mcp_tool',
              args_schema: { properties: { foo: { type: 'string' }, neverSaved: { type: 'string' } }, required: ['foo'] },
            },
          ],
        },
      },
    ];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="McpNode"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.requiredInputs).toEqual(['foo']));
    setYamlJsonObject.mockClear();

    // 'neverSaved' is optional, cleared to '', and was never a key of the node's saved input_mapping.
    act(() => {
      latest?.onChangeMapping('neverSaved', { type: 'fixed', value: '' });
    });

    expect(setYamlJsonObject).toHaveBeenCalledTimes(1);
    const written = setYamlJsonObject.mock.calls[0]?.[0] as YamlPipelineDocument;
    const writtenMapping = written.nodes?.find(node => node.id === 'McpNode')?.input_mapping as Record<string, unknown> | undefined;
    // The original 'foo' entry survives untouched, and the write path used was the generic
    // variable-update helper (not the delete branch, since there was nothing to delete).
    expect(writtenMapping).toHaveProperty('foo');
  });

  it('initial default-mapping write filters out an optional empty-valued field while keeping one with a non-empty default', async () => {
    const yamlJsonObject: YamlPipelineDocument = {
      nodes: [{ id: 'McpNode', tool: 'my_mcp_tool', toolkit_name: 'my_mcp_toolkit' }],
    };
    const setYamlJsonObject = vi.fn();
    const versionTools = [
      {
        type: 'mcp',
        name: 'my_mcp_toolkit',
        toolkit_name: 'my_mcp_toolkit',
        settings: {
          available_mcp_tools: [
            {
              value: 'my_mcp_tool',
              args_schema: {
                properties: {
                  foo: { type: 'string' }, // required -- always kept
                  emptyOptional: { type: 'string' }, // optional, no default -> value '' -> filtered out
                  presetOptional: { type: 'string', default: 'preset' }, // optional, has a default -> kept
                },
                required: ['foo'],
              },
            },
          ],
        },
      },
    ];

    let latest: UseFunctionInputMappingResult | undefined;
    renderWithRouterAndProject(
      <HookProbe
        id="McpNode"
        yamlJsonObject={yamlJsonObject}
        setYamlJsonObject={setYamlJsonObject}
        versionTools={versionTools}
        onResult={result => {
          latest = result;
        }}
      />,
      PROJECT_ID,
    );

    await waitFor(() => expect(latest?.requiredInputs).toEqual(['foo']));

    await waitFor(() => {
      const written = setYamlJsonObject.mock.calls.at(-1)?.[0] as YamlPipelineDocument;
      const writtenMapping = written.nodes?.find(node => node.id === 'McpNode')?.input_mapping;
      expect(writtenMapping).toHaveProperty('presetOptional');
    });
    const finalWritten = setYamlJsonObject.mock.calls.at(-1)?.[0] as YamlPipelineDocument;
    const finalMapping = finalWritten.nodes?.find(node => node.id === 'McpNode')?.input_mapping;
    expect(finalMapping).toHaveProperty('foo');
    expect(finalMapping).not.toHaveProperty('emptyOptional');
  });

  /**
   * #440. `dynamicToolNames` was a frozen empty array, so a toolkit that
   * publishes its tools at run time left every node's tool picker empty, and
   * a failed read looked exactly the same. The cases below discriminate the
   * three outcomes: a list, a failure, and a real empty catalogue.
   */
  describe('dynamic tool catalogue (#440)', () => {
    const DISCOVER = `${BASE}/elitea_core/toolkit_discover_tools/prompt_lib/${PROJECT_ID}/:toolkitType`;
    const yamlJsonObject: YamlPipelineDocument = { nodes: [{ id: 'Node1', toolkit_name: 'custom_toolkit' }] };
    const versionTools: readonly VersionTool[] = [{ type: 'custom', name: 'custom_toolkit', toolkit_name: 'custom_toolkit' }];

    function renderProbe(tools: readonly VersionTool[] = versionTools): { readonly read: () => UseFunctionInputMappingResult | undefined } {
      let latest: UseFunctionInputMappingResult | undefined;
      renderWithRouterAndProject(
        <HookProbe
          id="Node1"
          yamlJsonObject={yamlJsonObject}
          setYamlJsonObject={vi.fn()}
          versionTools={tools}
          onResult={result => {
            latest = result;
          }}
        />,
        PROJECT_ID,
      );
      return { read: () => latest };
    }

    it('reports the tool names the backend publishes for the selected toolkit', async () => {
      server.use(http.post(DISCOVER, () => HttpResponse.json({ tools: [{ id: '1', name: 'alpha_op', type: 'custom' }], total: 1 })));

      const { read } = renderProbe();

      await waitFor(() => expect(read()?.dynamicToolNames).toEqual(['alpha_op']));
      expect(read()?.dynamicToolsReadFailed).toBe(false);
    });

    it('reports a failed read as its own signal, not as an empty tool list', async () => {
      server.use(http.post(DISCOVER, () => HttpResponse.json({ error: 'read available tools failed' }, { status: 500 })));

      const { read } = renderProbe();

      await waitFor(() => expect(read()?.dynamicToolsReadFailed).toBe(true));
      expect(read()?.dynamicToolNames).toEqual([]);
    });

    it('reports a successful read with no tools as an empty list, not as a failure', async () => {
      let requestCount = 0;
      server.use(
        http.post(DISCOVER, () => {
          requestCount += 1;
          return HttpResponse.json({ tools: [], total: 0 });
        }),
      );

      const { read } = renderProbe();

      await waitFor(() => expect(requestCount).toBe(1));
      await waitFor(() => expect(read()?.dynamicToolsReadFailed).toBe(false));
      expect(read()?.dynamicToolNames).toEqual([]);
    });

    it('does not read the catalogue for a toolkit that carries its own selected_tools', async () => {
      let requestCount = 0;
      server.use(
        http.post(DISCOVER, () => {
          requestCount += 1;
          return HttpResponse.json({ tools: [{ id: '1', name: 'should_not_appear', type: 'custom' }], total: 1 });
        }),
      );

      const { read } = renderProbe([{ type: 'custom', name: 'custom_toolkit', toolkit_name: 'custom_toolkit', settings: { selected_tools: ['explicit_op'] } }]);

      await waitFor(() => expect(read()?.selectedToolkit).toBeDefined());
      expect(requestCount).toBe(0);
      expect(read()?.dynamicToolNames).toEqual([]);
    });
  });
});
