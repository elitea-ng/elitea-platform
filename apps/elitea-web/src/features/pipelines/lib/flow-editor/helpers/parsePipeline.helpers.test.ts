import { load } from 'js-yaml';
import { describe, expect, it } from 'vitest';

import { serializePipelineYaml } from '../../dumpYaml.helpers';

import {
  extractPipelineNodeTypes,
  migerateLegacyNodes,
  parseNodes,
  parseState,
  parseYaml,
} from './parsePipeline.helpers';
import { PipelineNodeTypes } from '../constants/flowEditor.constants';
import type { YamlPipelineDocument } from './pipelineFlow.types';

describe('re-exports (single `parsePipeline.helpers` import surface)', () => {
  it('parseState and parseNodes are the real functions from their split-out modules', () => {
    expect(parseState({ state: { input: 'str' } })).not.toBeNull();
    expect(parseNodes(undefined).nodes).toHaveLength(1);
  });
});

describe('parseYaml', () => {
  it('combines parseState + parseNodes and re-tags unrecognised node types as Default', () => {
    const doc: YamlPipelineDocument = {
      entry_point: 'A',
      nodes: [{ id: 'A', type: 'some_custom_dsl_type', transition: 'END' }],
    };
    const result = parseYaml(doc);
    const custom = result.nodes.find(n => n.id === 'A');
    expect(custom?.type).toBe(PipelineNodeTypes.Default);
    expect(custom?.['originalEliteAType']).toBe('some_custom_dsl_type');
    expect(custom?.data?.['type']).toBe('some_custom_dsl_type');
  });

  it('leaves a recognised node type alone', () => {
    const doc: YamlPipelineDocument = { entry_point: 'A', nodes: [{ id: 'A', type: 'tool', transition: 'END' }] };
    const result = parseYaml(doc);
    expect(result.nodes.find(n => n.id === 'A')?.type).toBe('tool');
  });
});

describe('migerateLegacyNodes', () => {
  it('returns the document verbatim when there are no nodes at all', () => {
    expect(migerateLegacyNodes(undefined)).toBeUndefined();
    expect(migerateLegacyNodes({ entry_point: 'A' })).toEqual({ entry_point: 'A' });
  });

  it('returns { yamlJson, flowNodesToRemove: [] } when nodes exist but none has a legacy decision', () => {
    const doc: YamlPipelineDocument = { nodes: [{ id: 'A', type: 'tool' }] };
    expect(migerateLegacyNodes(doc)).toEqual({ yamlJson: doc, flowNodesToRemove: [] });
  });

  it('extracts a legacy inline decision into a standalone Decision node with a transition pointing to it', () => {
    const doc: YamlPipelineDocument = {
      nodes: [{ id: 'A', type: 'tool', decision: { nodes: ['B'], default_output: 'C', decisional_inputs: ['x'] } }],
    };
    const result = migerateLegacyNodes(doc) as { yamlJson: YamlPipelineDocument; flowNodesToRemove: readonly string[] };
    expect(result.flowNodesToRemove).toEqual(['A~~~DecisionNode']);
    const migratedA = result.yamlJson.nodes?.find(n => n.id === 'A');
    expect(migratedA).not.toHaveProperty('decision');
    expect(migratedA?.transition).toBeTypeOf('string');
    const decisionNode = result.yamlJson.nodes?.find(n => n.type === PipelineNodeTypes.Decision);
    expect(decisionNode).toMatchObject({ nodes: ['B'], default_output: 'C', input: ['x'] });
    expect(decisionNode).not.toHaveProperty('decisional_inputs');
  });

  it('writes no undefined input for a legacy decision without decisional_inputs, so the result serializes', () => {
    const doc: YamlPipelineDocument = {
      entry_point: 'Decision_1',
      nodes: [{ id: 'Decision_1', decision: { nodes: ['LLM_1'] } }, { id: 'LLM_1', type: 'llm', transition: 'END' }],
    };
    const { yamlJson } = migerateLegacyNodes(doc) as { yamlJson: YamlPipelineDocument };
    const decisionNode = yamlJson.nodes?.find(n => n.type === PipelineNodeTypes.Decision);
    expect(decisionNode).not.toHaveProperty('input');
    expect(load(serializePipelineYaml(yamlJson))).toStrictEqual(yamlJson);
  });

  it('leaves non-decision nodes untouched while migrating the ones that need it', () => {
    const doc: YamlPipelineDocument = {
      nodes: [{ id: 'Plain', type: 'tool' }, { id: 'A', type: 'tool', decision: { nodes: ['B'] } }],
    };
    const result = migerateLegacyNodes(doc) as { yamlJson: YamlPipelineDocument };
    expect(result.yamlJson.nodes?.find(n => n.id === 'Plain')).toEqual({ id: 'Plain', type: 'tool' });
  });
});

describe('extractPipelineNodeTypes', () => {
  it('counts nodes by type and totals them', () => {
    const yaml = 'nodes:\n  - id: A\n    type: tool\n  - id: B\n    type: tool\n  - id: C\n    type: agent\n';
    expect(extractPipelineNodeTypes(yaml)).toEqual({ nodeTypes: { tool: 2, agent: 1 }, totalNodeCount: 3 });
  });

  it('returns null for empty/missing instructions', () => {
    expect(extractPipelineNodeTypes(null)).toBeNull();
    expect(extractPipelineNodeTypes('')).toBeNull();
  });

  it('returns null when parsed YAML has no nodes array', () => {
    expect(extractPipelineNodeTypes('entry_point: A')).toBeNull();
  });

  it('returns null (not throw) on malformed YAML', () => {
    expect(extractPipelineNodeTypes('nodes: [unterminated')).toBeNull();
  });

  it('skips nodes with no `type` field', () => {
    const yaml = 'nodes:\n  - id: A\n  - id: B\n    type: tool\n';
    expect(extractPipelineNodeTypes(yaml)).toEqual({ nodeTypes: { tool: 1 }, totalNodeCount: 1 });
  });

  it('skips a `null` entry in the nodes array instead of discarding the whole histogram (regression)', () => {
    const yaml = 'nodes:\n  - null\n  - id: B\n    type: tool\n';
    expect(extractPipelineNodeTypes(yaml)).toEqual({ nodeTypes: { tool: 1 }, totalNodeCount: 1 });
  });
});
