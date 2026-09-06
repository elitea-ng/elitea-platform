import { load } from 'js-yaml';
import { describe, expect, it } from 'vitest';

import type { YamlPipelineDocument } from './flow-editor/helpers/pipelineFlow.types';
import { collectGraphAdmissionIssues } from './graphAdmission.helpers';
import { PipelineNodeTypes } from './flow-editor/constants/flowEditor.constants';
import { getInitialNodeId } from './flow-editor/helpers/nodeIdentity.helpers';
import { PIPELINE_STARTER_ENTRY_NODE_ID, PIPELINE_STARTER_TEMPLATE } from '@/shared/lib/pipelineStarterTemplate';

function starterDocument(): YamlPipelineDocument {
  return load(PIPELINE_STARTER_TEMPLATE) as YamlPipelineDocument;
}

describe('the pipeline starter template', () => {
  it('parses as YAML', () => {
    expect(starterDocument()).toBeTypeOf('object');
  });

  it('is admitted whole by the runtime graph rules', () => {
    // The point of the template: a pipeline created from it RUNS as created.
    // `collectGraphAdmissionIssues` mirrors the Rust compiler, and one issue
    // refuses the whole document.
    expect(collectGraphAdmissionIssues(starterDocument())).toEqual([]);
  });

  it('wires its single LLM node to the entry point and to END', () => {
    const document = starterDocument();

    expect(document.entry_point).toBe(PIPELINE_STARTER_ENTRY_NODE_ID);
    expect(document.nodes).toHaveLength(1);
    expect(document.nodes?.[0]?.id).toBe(PIPELINE_STARTER_ENTRY_NODE_ID);
    expect(document.nodes?.[0]?.type).toBe('llm');
    expect(document.nodes?.[0]?.transition).toBe('END');
  });

  it('maps the user turn into the node, not the empty messages channel', () => {
    // The Python worker's SDK moves the user's message out of `messages` and
    // into the `input` state variable before the graph runs, so a node mapped
    // only to `messages` raises "LLMNode requires 'messages' in state". The
    // SDK also refuses a pipeline LLM node that has no `system`.
    const mapping = starterDocument().nodes?.[0]?.input_mapping;

    expect(mapping?.['system']?.type).toBe('fstring');
    expect(mapping?.['task']).toEqual({ type: 'variable', value: 'input' });
    expect(starterDocument().nodes?.[0]?.input).toEqual(['input']);
  });

  it('makes the editor mint LLM_2 for the next LLM node the author adds', () => {
    // Consequence of shipping a template, and the one that is easy to miss:
    // a new pipeline no longer opens on an empty canvas, so `LLM_1` is
    // already taken. Any caller that reads back "the LLM node" of a pipeline
    // by name prefix now gets the STARTER's node, not the author's.
    // `e2e/journeys/pipelines/pipelines.versioning.spec.ts` (J16b) did exactly
    // that and read the base version's own starter node as an overwrite.
    const document = starterDocument();

    expect(getInitialNodeId(PipelineNodeTypes.LLM, document.nodes ?? [])).toBe('LLM_2');
    // On an empty canvas — the state before the template — it was `LLM_1`.
    expect(getInitialNodeId(PipelineNodeTypes.LLM, [])).toBe(PIPELINE_STARTER_ENTRY_NODE_ID);
  });

  it('declares every state variable its node reads and writes', () => {
    const document = starterDocument();

    expect(document.state).toEqual({ input: 'str', messages: 'list' });
    expect(document.nodes?.[0]?.output).toEqual(['messages']);
  });
});
