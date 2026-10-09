import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { load } from 'js-yaml';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { parseYamlToMermaid } from '@/features/agents/lib/helpers/parseYamlToMermaid.helpers';
import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';

import { renderHookWithProviders, renderWithRouterAndProject } from '../__tests__/testUtils';
import { usePipelineEditorStore } from '../model/pipelineEditorStore';
import { usePipelineYamlStore } from '../model/pipelineYamlStore';
import { EditorPanel } from '../ui/EditorPanel';
import { usePipelineVersionSync } from '../ui/usePipelineEditorLifecycle';
import { migerateLegacyNodes, parseYaml } from './flow-editor/helpers/parsePipeline.helpers';
import type { YamlPipelineDocument } from './flow-editor/helpers/pipelineFlow.types';
import { collectGraphAdmissionIssues } from './graphAdmission.helpers';
import { computeIsPipelineYamlCodeDirty } from './hooks/useIsPipelineYamlCodeDirty';
import { judgeLivePipelineGraph } from './livePipelineGraphAdmission';
import { dumpYaml } from './dumpYaml.helpers';
import { isIdentifierRespellingRedump, parsePipelineYamlDocument } from './pipelineYamlDocument.helpers';

installCodeMirrorTestPolyfills();

/** Unquoted numeric ids, hand-formatted (flow style) so any silent re-dump would change the text. */
const NUMERIC_YAML = `entry_point: 1
nodes:
  - {id: 1, type: llm, transition: 2}
  - {id: 2, type: router, routes: [3, 4], default_output: 3}
  - {id: 3, type: llm, transition: END}
  - {id: 4, type: hitl, routes: {approve: 3}}
`;
const PIPELINE_PATH = '/pipelines/latest/1';

function doc(source: string): YamlPipelineDocument {
  return load(source) as YamlPipelineDocument;
}

const edgePairs = (edges: readonly { source: string; target: string }[]): string[] => edges.map((edge) => `${edge.source}>${edge.target}`);

beforeEach(() => {
  usePipelineYamlStore.setState({
    yamlCode: '',
    yamlJsonObject: {},
    initYamlCode: '',
    initYamlJsonObject: {},
    resetFlag: false,
    layoutVersion: undefined,
  });
  usePipelineEditorStore.setState({ nodes: [], edges: [], stateValidationErrors: {} });
});

describe('numeric node ids: parsing', () => {
  it('parseYaml yields string node ids and 1>2 edges for unquoted numeric ids', () => {
    const { nodes, edges } = parseYaml(doc(NUMERIC_YAML));
    expect(nodes.map((node) => node.id)).toEqual(['END', '1', '2', '3', '4']);
    nodes.forEach((node) => expect(typeof node.id).toBe('string'));
    expect(edgePairs(edges)).toEqual(expect.arrayContaining(['1>2', '2>3', '2>4', '3>END', '4>3']));
    edges.forEach((edge) => {
      expect(typeof edge.source).toBe('string');
      expect(typeof edge.target).toBe('string');
    });
  });

  it('keeps a node whose id is 0 (an unnormalized 0 is falsy and was dropped as an empty route)', () => {
    const { nodes, edges } = parseYaml(doc('entry_point: 1\nnodes:\n  - {id: 1, type: llm, transition: 0}\n  - {id: 0, type: llm, transition: END}\n'));
    expect(nodes.map((node) => node.id)).toEqual(['END', '1', '0']);
    expect(edgePairs(edges)).toContain('1>0');
  });

  it('migerateLegacyNodes no longer crashes with "id.replace is not a function" on a legacy decision with numeric ids', () => {
    const legacy = doc('entry_point: 1\nnodes:\n  - id: 1\n    decision: {nodes: [2]}\n  - {id: 2, type: llm, transition: END}\n');
    const result = migerateLegacyNodes(legacy) as { yamlJson: YamlPipelineDocument; flowNodesToRemove: string[] };
    expect(result.flowNodesToRemove).toEqual(['1~~~DecisionNode']);
    expect(result.yamlJson.entry_point).toBe('1');
    expect(result.yamlJson.nodes?.map((node) => node.id)).toEqual(['1', 'Decision_1', '2']);
  });

  it('migerateLegacyNodes hands the editor a normalized document even when nothing is migrated', () => {
    const result = migerateLegacyNodes(doc(NUMERIC_YAML)) as { yamlJson: YamlPipelineDocument; flowNodesToRemove: string[] };
    expect(result.yamlJson.entry_point).toBe('1');
    expect(result.yamlJson.nodes?.[0]?.transition).toBe('2');
  });

  it('the pipeline YAML store parses a numeric-id document into string ids but keeps the source text', () => {
    const snapshot = parsePipelineYamlDocument(NUMERIC_YAML);
    expect(snapshot.yamlCode).toBe(NUMERIC_YAML);
    expect(snapshot.yamlJsonObject['entry_point']).toBe('1');
  });

  it('agent context diagram (parseYamlToMermaid) renders numeric ids instead of throwing "id?.replace is not a function"', () => {
    const diagram = parseYamlToMermaid(NUMERIC_YAML);
    expect(diagram).toContain('start --> 1');
    expect(diagram).toContain('1 --> 2');
  });
});

describe('numeric node ids: the store keeps the author\'s text', () => {
  const seeded = (): void => {
    const parsed = parsePipelineYamlDocument(NUMERIC_YAML);
    usePipelineYamlStore.getState().initPipelineYaml({ yamlCode: NUMERIC_YAML, yamlJsonObject: parsed.yamlJsonObject });
  };

  it('ignores the editor re-dumping the same document with quoted ids', () => {
    seeded();
    const redump = dumpYaml(usePipelineYamlStore.getState().yamlJsonObject);
    expect(redump).not.toBe(NUMERIC_YAML);
    expect(isIdentifierRespellingRedump(usePipelineYamlStore.getState(), redump)).toBe(true);
    usePipelineYamlStore.getState().setYamlCode(redump);
    expect(usePipelineYamlStore.getState().yamlCode).toBe(NUMERIC_YAML);
  });

  it('stores anything a person types, including a respelling of an id', () => {
    seeded();
    const typed = NUMERIC_YAML.replace('{id: 1,', "{id: '1',");
    usePipelineYamlStore.getState().setYamlCode(typed);
    expect(usePipelineYamlStore.getState().yamlCode).toBe(typed);
  });

  it('stores the dump of a document that really changed', () => {
    seeded();
    const changed = { ...usePipelineYamlStore.getState().yamlJsonObject, entry_point: '2' };
    usePipelineYamlStore.getState().setYamlCode(dumpYaml(changed));
    expect(usePipelineYamlStore.getState().yamlCode).toBe(dumpYaml(changed));
  });

  it('never withholds the dump for a pipeline that has no numeric ids', () => {
    const plain = 'entry_point: A\nnodes:\n  - {id: A, type: llm, transition: END}\n';
    const parsed = parsePipelineYamlDocument(plain);
    usePipelineYamlStore.getState().initPipelineYaml({ yamlCode: plain, yamlJsonObject: parsed.yamlJsonObject });
    const redump = dumpYaml(parsed.yamlJsonObject);
    expect(isIdentifierRespellingRedump(usePipelineYamlStore.getState(), redump)).toBe(false);
    usePipelineYamlStore.getState().setYamlCode(redump);
    expect(usePipelineYamlStore.getState().yamlCode).toBe(redump);
  });

  it('does not throw while the stored text is not valid YAML', () => {
    usePipelineYamlStore.setState({ yamlCode: 'nodes: [', yamlJsonObject: {} });
    expect(isIdentifierRespellingRedump(usePipelineYamlStore.getState(), 'nodes: []')).toBe(false);
  });
});

describe('numeric node ids: admission (the existing editor validation)', () => {
  it('admits a well-formed numeric-id pipeline', () => {
    expect(judgeLivePipelineGraph(NUMERIC_YAML)).toMatchObject({ isAdmissible: true, parseFailed: false, issues: [] });
    expect(collectGraphAdmissionIssues(doc(NUMERIC_YAML))).toEqual([]);
  });

  it('still reports a numeric transition that names no node', () => {
    const issues = judgeLivePipelineGraph('entry_point: 1\nnodes:\n  - {id: 1, type: llm, transition: 9}\n').issues;
    expect(issues.map((issue) => `${issue.rule}:${issue.field}:${issue.subject}`)).toEqual(['node.route-target:transition:9']);
  });

  it.each([
    ['entry_point', 'entry_point: 1.5\nnodes:\n  - {id: 1, type: llm, transition: END}\n', 'document.entry-point'],
    ['bool id', 'entry_point: 1\nnodes:\n  - {id: true, type: llm, transition: END}\n', 'node.id'],
    ['unsafe id', 'entry_point: 1\nnodes:\n  - {id: 9007199254740992, type: llm, transition: END}\n', 'node.id'],
    ['fractional interrupt', 'entry_point: 1\ninterrupt_before: [1.5]\nnodes:\n  - {id: 1, type: llm, transition: END}\n', 'document.static-interrupts'],
    ['bool transition', 'entry_point: 1\nnodes:\n  - {id: 1, type: llm, transition: true}\n', 'node.route-target'],
    ['fractional transition', 'entry_point: 1\nnodes:\n  - {id: 1, type: llm, transition: 2.5}\n  - {id: 2, type: llm, transition: END}\n', 'node.route-target'],
    ['object route', 'entry_point: 1\nnodes:\n  - {id: 1, type: router, routes: [{a: 1}], default_output: END}\n', 'node.route-target'],
    ['unsafe decision target', 'entry_point: 1\nnodes:\n  - {id: 1, type: decision, nodes: [9007199254740993]}\n', 'node.route-target'],
    ['bool hitl route', 'entry_point: 1\nnodes:\n  - {id: 1, type: hitl, routes: {approve: false}}\n', 'node.route-target'],
  ])('reports %s through admission and does not throw', (_name, source, rule) => {
    const verdict = judgeLivePipelineGraph(source);
    expect(verdict.isAdmissible).toBe(false);
    expect(verdict.issues.map((issue) => issue.rule)).toContain(rule);
    expect(() => parseYaml(doc(source))).not.toThrow();
    expect(() => parseYamlToMermaid(source)).not.toThrow();
  });

  it('keeps today\'s handling of null and absent optional fields (no issue, no crash)', () => {
    const source = 'entry_point: 1\nnodes:\n  - {id: 1, type: router, routes: [2, null], default_output: null}\n  - {id: 2, type: llm, transition: null}\n';
    const rules = judgeLivePipelineGraph(source).issues.map((issue) => issue.rule);
    expect(rules).not.toContain('node.route-target');
    expect(() => parseYaml(doc(source))).not.toThrow();
  });
});

describe('numeric node ids: loading a saved version', () => {
  function loadVersion(instructions: string): void {
    renderHookWithProviders(() =>
      usePipelineVersionSync({
        isCreateMode: false,
        versionDetails: { id: '7', application_id: '3', name: 'v1', status: 'draft', instructions },
        versionId: 7,
      }),
    );
  }

  it('seeds the canvas with string ids and edges, leaves the YAML text byte-identical, and is not dirty', () => {
    loadVersion(NUMERIC_YAML);

    const yaml = usePipelineYamlStore.getState();
    expect(yaml.yamlCode).toBe(NUMERIC_YAML);
    expect(yaml.initYamlCode).toBe(NUMERIC_YAML);
    expect(yaml.yamlJsonObject['entry_point']).toBe('1');
    expect(yaml.initYamlJsonObject['entry_point']).toBe('1');
    expect(computeIsPipelineYamlCodeDirty(PIPELINE_PATH, yaml.yamlCode, yaml.initYamlCode)).toBe(false);

    const editor = usePipelineEditorStore.getState();
    expect(editor.nodes.map((node) => node.id).sort()).toEqual(['1', '2', '3', '4', 'END']);
    expect(edgePairs(editor.edges as never)).toEqual(expect.arrayContaining(['1>2', '2>3', '2>4', '4>3']));
  });
});

describe('numeric node ids: the real editor panel', () => {
  function seedFromYaml(source: string): void {
    renderHookWithProviders(() =>
      usePipelineVersionSync({
        isCreateMode: false,
        versionDetails: { id: '7', application_id: '3', name: 'v1', status: 'draft', instructions: source },
        versionId: 7,
      }),
    );
  }

  it('renders string-id nodes, switches Yaml and Flow without rewriting or dirtying the YAML, and keeps edges connected after an edit', async () => {
    seedFromYaml(NUMERIC_YAML);
    const setYamlDirty = vi.fn();
    const { container } = renderWithRouterAndProject(<EditorPanel setYamlDirty={setYamlDirty} stopRun={vi.fn()} />, '1');
    const user = userEvent.setup();

    await waitFor(() => expect(container.querySelector('.react-flow__node[data-id="2"]')).not.toBeNull(), { timeout: 20000 });
    expect(screen.queryByText('Failed to load the flow editor')).not.toBeInTheDocument();

    // Non-editing interactions: view switches must not touch the stored text or arm the guard.
    await user.click(await screen.findByRole('button', { name: 'Yaml' }));
    await user.click(await screen.findByRole('button', { name: 'Flow' }));
    await waitFor(() => expect(container.querySelector('.react-flow__node[data-id="1"]')).not.toBeNull());
    const afterSwitch = usePipelineYamlStore.getState();
    expect(afterSwitch.yamlCode).toBe(NUMERIC_YAML);
    expect(computeIsPipelineYamlCodeDirty(PIPELINE_PATH, afterSwitch.yamlCode, afterSwitch.initYamlCode)).toBe(false);
    expect(setYamlDirty).not.toHaveBeenCalledWith(true);

    // A real edit: add a node. Whatever the dump path writes must still parse, with 1 > 2 intact.
    await user.click(await screen.findByRole('button', { name: 'Add node' }));
    await user.click(await screen.findByRole('menuitem', { name: 'Agent' }));
    await waitFor(() => expect(usePipelineYamlStore.getState().yamlCode).not.toBe(NUMERIC_YAML));

    const edited = usePipelineYamlStore.getState();
    const reparsed = parseYaml(doc(edited.yamlCode));
    expect(reparsed.nodes.map((node) => node.id)).toEqual(expect.arrayContaining(['1', '2', '3', '4', 'Agent_1']));
    expect(edgePairs(reparsed.edges)).toEqual(expect.arrayContaining(['1>2', '2>3', '2>4', '4>3']));
    expect(judgeLivePipelineGraph(edited.yamlCode).issues.map((issue) => issue.field)).not.toContain('entry_point');
    expect(computeIsPipelineYamlCodeDirty(PIPELINE_PATH, edited.yamlCode, edited.initYamlCode)).toBe(true);
    await act(async () => {
      await Promise.resolve();
    });
  }, 30000);
});
