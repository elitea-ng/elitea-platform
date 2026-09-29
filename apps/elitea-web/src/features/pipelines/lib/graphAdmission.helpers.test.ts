/**
 * One case per admission rule, each naming the `services/elitea-worker-rust/
 * src/agents/graph/` line it encodes.
 *
 * The suite opens with two cases that are not about any single rule, and
 * both are load-bearing:
 *
 *  - **the catalogue is complete** — a rule silently dropped from
 *    `GRAPH_ADMISSION_RULES` would take its whole class of refusals with it,
 *    and every "this document is rejected" case below would still pass on
 *    the strength of some other rule. The id list is asserted verbatim.
 *  - **the admissible baseline stays admissible** — without it, a rule that
 *    fires on everything satisfies every negative case in this file. That is
 *    the same trap the editor is being built to close, one level up: a
 *    refusal nobody can act on is worse than no refusal.
 */
import { describe, expect, it } from 'vitest';

import type { YamlPipelineDocument, YamlPipelineNode } from './flow-editor/helpers/pipelineFlow.types';
import { GRAPH_ADMISSION_RULES, collectGraphAdmissionIssues, documentLevelIssues, issuesForNode } from './graphAdmission.helpers';
import type { GraphAdmissionRuleId } from './graphAdmission.types';

/** An LLM node the compiler admits: one declared, non-`messages` output, a legal id, a legal transition. */
const ADMISSIBLE_LLM_NODE: YamlPipelineNode = {
  id: 'LLM_1',
  type: 'llm',
  input: ['messages'],
  output: ['summary'],
  transition: 'END',
};

/** The smallest document `PipelineDefinition::from_yaml` accepts, with one state variable to point at. */
function baseDocument(overrides: Partial<YamlPipelineDocument> = {}): YamlPipelineDocument {
  return {
    state: { input: 'str', messages: 'list', summary: 'str' },
    entry_point: 'LLM_1',
    nodes: [ADMISSIBLE_LLM_NODE],
    ...overrides,
  };
}

/** A document whose single node is `ADMISSIBLE_LLM_NODE` with `patch` applied. */
function withNode(patch: Partial<YamlPipelineNode>, overrides: Partial<YamlPipelineDocument> = {}): YamlPipelineDocument {
  return baseDocument({ nodes: [{ ...ADMISSIBLE_LLM_NODE, ...patch }], ...overrides });
}

/** Every rule id an admission pass over `document` reported. */
function ruleIds(document: YamlPipelineDocument): readonly GraphAdmissionRuleId[] {
  return collectGraphAdmissionIssues(document).map((issue) => issue.rule);
}

describe('the rule catalogue', () => {
  it('holds every rule, in compiler order', () => {
    expect(GRAPH_ADMISSION_RULES.map((rule) => rule.id)).toEqual([
      'document.node-count',
      'document.entry-point',
      'document.static-interrupts',
      'state.key',
      'state.type',
      'state.builtin-type',
      'node.type',
      'node.id',
      'node.required-field',
      'node.route-target',
      'node.state-reference',
      'node.structured-output',
    ]);
  });

  it('gives every rule a runtime citation', () => {
    for (const rule of GRAPH_ADMISSION_RULES) {
      expect(rule.citation, rule.id).toMatch(/\.rs:\d+/);
    }
  });

  it('admits the baseline document — no rule fires on a legal graph', () => {
    expect(collectGraphAdmissionIssues(baseDocument())).toEqual([]);
  });
});

describe('document rules', () => {
  it('document.node-count: refuses an empty pipeline (compiler.rs:459)', () => {
    const issues = collectGraphAdmissionIssues(baseDocument({ nodes: [] }));

    expect(issues.map((issue) => issue.rule)).toContain('document.node-count');
    expect(issues[0]?.citation).toBe('compiler.rs:459');
  });

  it('document.entry-point: refuses an entry point that names no node (compiler.rs:477)', () => {
    const issues = documentLevelIssues(collectGraphAdmissionIssues(baseDocument({ entry_point: 'Start_Here' })));

    expect(issues[0]?.rule).toBe('document.entry-point');
    expect(issues[0]?.subject).toBe('Start_Here');
    expect(issues[0]?.message).toContain('does not name any node');
    expect(issues[0]?.citation).toBe('compiler.rs:477');
  });

  it('document.entry-point: refuses a malformed entry point (compiler.rs:464)', () => {
    const issues = collectGraphAdmissionIssues(baseDocument({ entry_point: 'LLM 1' }));

    expect(issues.map((issue) => issue.citation)).toContain('compiler.rs:464');
  });

  it('document.static-interrupts: refuses any static interrupt (compiler.rs:470)', () => {
    const issues = documentLevelIssues(collectGraphAdmissionIssues(baseDocument({ interrupt_before: ['LLM_1'] })));

    expect(issues[0]?.rule).toBe('document.static-interrupts');
    expect(issues[0]?.field).toBe('interrupt_before');
    expect(issues[0]?.message).toContain('not supported by the native runtime');
    expect(issues[0]?.citation).toBe('compiler.rs:470');
  });

  it('document.static-interrupts: an empty interrupt list is fine (compiler.rs:470 tests `is_empty`)', () => {
    expect(collectGraphAdmissionIssues(baseDocument({ interrupt_before: [], interrupt_after: [] }))).toEqual([]);
  });
});

describe('state rules', () => {
  it('state.key: refuses a reserved state key (compiler.rs:1373 -> compiler.rs:1470)', () => {
    const issues = collectGraphAdmissionIssues(baseDocument({ state: { input: 'str', messages: 'list', output: 'str' } }));

    expect(issues.map((issue) => issue.rule)).toContain('state.key');
    expect(issues.find((issue) => issue.rule === 'state.key')?.subject).toBe('output');
  });

  it('state.key: `input` and `messages` are built-in but NOT reserved (compiler.rs:1436 vs :1456)', () => {
    expect(ruleIds(baseDocument())).not.toContain('state.key');
  });

  it('state.type: refuses a type outside the six the compiler normalises (compiler.rs:1386)', () => {
    const issues = collectGraphAdmissionIssues(baseDocument({ state: { input: 'str', messages: 'list', summary: 'datetime' } }));

    const typeIssue = issues.find((issue) => issue.rule === 'state.type');
    expect(typeIssue?.field).toBe('state.summary');
    expect(typeIssue?.subject).toBe('datetime');
    expect(typeIssue?.citation).toBe('compiler.rs:1386');
  });

  it('state.builtin-type: `input` must be declared str (compiler.rs:1391)', () => {
    const issues = collectGraphAdmissionIssues(baseDocument({ state: { input: 'list', messages: 'list', summary: 'str' } }));

    const pinned = issues.find((issue) => issue.rule === 'state.builtin-type');
    expect(pinned?.field).toBe('state.input');
    expect(pinned?.message).toContain('"str"');
    expect(pinned?.citation).toBe('compiler.rs:1391');
  });
});

describe('node rules', () => {
  it('node.type: refuses a type with no `parse_pipeline_node` arm (compiler.rs:1267)', () => {
    const issues = issuesForNode(collectGraphAdmissionIssues(withNode({ type: 'custom' })), 'LLM_1');

    expect(issues[0]?.rule).toBe('node.type');
    expect(issues[0]?.subject).toBe('custom');
    expect(issues[0]?.citation).toBe('compiler.rs:1267');
  });

  it('admits a saved Python Code node with a message output', () => {
    const document = baseDocument({
      entry_point: 'execute',
      nodes: [{ id: 'execute', type: 'code', language: 'python', code: { type: 'fixed', value: 'result = 42' }, output: ['messages'], transition: 'END' }],
    });
    expect(collectGraphAdmissionIssues(document)).toEqual([]);
  });

  it('node.id: refuses a space in a node id (yaml.rs:362)', () => {
    const issues = collectGraphAdmissionIssues(withNode({ id: 'LLM 1' }, { entry_point: 'LLM 1' }));

    const idIssue = issues.find((issue) => issue.rule === 'node.id');
    expect(idIssue?.subject).toBe('LLM 1');
    expect(idIssue?.message).toContain('a space is not');
    expect(idIssue?.citation).toBe('yaml.rs:362');
  });

  it('node.id: refuses duplicate ids (compiler.rs:1225)', () => {
    const issues = collectGraphAdmissionIssues(baseDocument({ nodes: [ADMISSIBLE_LLM_NODE, ADMISSIBLE_LLM_NODE] }));

    expect(issues.find((issue) => issue.rule === 'node.id')?.citation).toBe('compiler.rs:1225');
  });

  it('node.id: refuses the compiler-owned reserved id (compiler.rs:1220)', () => {
    const reserved = '__elitea_subgraph_result_v1';
    const issues = collectGraphAdmissionIssues(withNode({ id: reserved }, { entry_point: reserved }));

    expect(issues.find((issue) => issue.rule === 'node.id')?.citation).toBe('compiler.rs:1220');
  });

  it('node.required-field: an Agent node needs a `tool` (application.rs:124)', () => {
    const agent: YamlPipelineNode = { id: 'Agent_1', type: 'agent', tool: '', input_mapping: { task: { type: 'fixed', value: '' } }, transition: 'END' };
    const issues = issuesForNode(collectGraphAdmissionIssues(baseDocument({ nodes: [agent], entry_point: 'Agent_1' })), 'Agent_1');

    expect(issues[0]?.rule).toBe('node.required-field');
    expect(issues[0]?.field).toBe('tool');
    expect(issues[0]?.citation).toBe('application.rs:124');
  });

  it('node.required-field: an Agent `input_mapping` holds exactly one `task` entry (application.rs:146)', () => {
    const agent: YamlPipelineNode = { id: 'Agent_1', type: 'agent', tool: 'writer', input_mapping: { query: { type: 'fixed', value: 'x' } }, transition: 'END' };
    const issues = issuesForNode(collectGraphAdmissionIssues(baseDocument({ nodes: [agent], entry_point: 'Agent_1' })), 'Agent_1');

    expect(issues[0]?.field).toBe('input_mapping');
    expect(issues[0]?.message).toContain('keyed "task"');
    expect(issues[0]?.citation).toBe('application.rs:146');
  });

  it('node.required-field: a Toolkit node needs a `toolkit_name` (direct_tool.rs:159)', () => {
    const toolkit: YamlPipelineNode = { id: 'Toolkit_1', type: 'toolkit', toolkit_name: '', tool: 'search', transition: 'END' };
    const issues = issuesForNode(collectGraphAdmissionIssues(baseDocument({ nodes: [toolkit], entry_point: 'Toolkit_1' })), 'Toolkit_1');

    expect(issues[0]?.field).toBe('toolkit_name');
    expect(issues[0]?.citation).toBe('direct_tool.rs:159');
  });

  it('node.required-field: a Printer node needs a `transition` (printer.rs:41)', () => {
    const printer: YamlPipelineNode = { id: 'Printer_1', type: 'printer' };
    const issues = issuesForNode(collectGraphAdmissionIssues(baseDocument({ nodes: [printer], entry_point: 'Printer_1' })), 'Printer_1');

    expect(issues[0]?.field).toBe('transition');
    expect(issues[0]?.citation).toBe('printer.rs:41');
  });

  it('node.required-field: a Decision node with an EMPTY `nodes` list is admitted (router.rs:70)', () => {
    const decision: YamlPipelineNode = { id: 'Decision_1', type: 'decision', nodes: [], default_output: 'END', input: ['messages'] };

    expect(collectGraphAdmissionIssues(baseDocument({ nodes: [decision], entry_point: 'Decision_1' }))).toEqual([]);
  });

  it('node.required-field: a HITL node with no executable action is refused (hitl.rs:474)', () => {
    const hitl: YamlPipelineNode = { id: 'HITL_1', type: 'hitl', input: ['messages'], routes: {} };
    const issues = issuesForNode(collectGraphAdmissionIssues(baseDocument({ nodes: [hitl], entry_point: 'HITL_1' })), 'HITL_1');

    expect(issues[0]?.field).toBe('routes');
    expect(issues[0]?.citation).toBe('hitl.rs:474');
  });

  it('node.required-field: a HITL `user_message` cleared to "" refuses the document (hitl.rs:440)', () => {
    // `validate_message` refuses `message.value.is_empty()` before anything
    // else about the node is read, and `compiler.rs:1252` turns that into "a
    // HITL node is invalid" for the WHOLE pipeline. Clearing the User
    // message field in the node card writes exactly this shape.
    const hitl: YamlPipelineNode = { id: 'HITL_1', type: 'hitl', input: ['messages'], user_message: { type: 'fixed', value: '' }, routes: { approve: 'END' } };
    const issues = issuesForNode(collectGraphAdmissionIssues(baseDocument({ nodes: [hitl], entry_point: 'HITL_1' })), 'HITL_1');

    expect(issues).toHaveLength(1);
    expect(issues[0]?.rule).toBe('node.required-field');
    expect(issues[0]?.field).toBe('user_message.value');
    expect(issues[0]?.citation).toBe('hitl.rs:440');
  });

  it('node.required-field: an ABSENT HITL `user_message` is legal — serde substitutes `default_message` (hitl.rs:60-64)', () => {
    // The discriminator for the rule above being a rule about the EMPTY
    // string and not about the field's presence. `#[serde(default)]` on the
    // struct and `#[serde(default = "default_message")]` on the field mean
    // an absent message compiles; refusing it would block a legal save.
    const hitl: YamlPipelineNode = { id: 'HITL_1', type: 'hitl', input: ['messages'], routes: { approve: 'END' } };

    expect(collectGraphAdmissionIssues(baseDocument({ nodes: [hitl], entry_point: 'HITL_1' }))).toEqual([]);
  });

  it('node.route-target: refuses a transition that names no node (compiler.rs:484)', () => {
    const issues = issuesForNode(collectGraphAdmissionIssues(withNode({ transition: 'Missing_1' })), 'LLM_1');

    expect(issues[0]?.rule).toBe('node.route-target');
    expect(issues[0]?.field).toBe('transition');
    expect(issues[0]?.subject).toBe('Missing_1');
    expect(issues[0]?.citation).toBe('compiler.rs:484');
  });

  it('node.route-target: refuses a malformed target, and END is always legal (router.rs:331, compiler.rs:484)', () => {
    expect(ruleIds(withNode({ transition: '' }))).toContain('node.route-target');
    expect(ruleIds(withNode({ transition: 'END' }))).not.toContain('node.route-target');
  });

  it('node.state-reference: refuses an output naming an undeclared state key (compiler.rs:1284)', () => {
    const issues = issuesForNode(collectGraphAdmissionIssues(withNode({ output: ['nowhere'] })), 'LLM_1');

    expect(issues[0]?.rule).toBe('node.state-reference');
    expect(issues[0]?.field).toBe('output[0]');
    expect(issues[0]?.subject).toBe('nowhere');
    expect(issues[0]?.message).toContain('"nowhere" is not declared');
    expect(issues[0]?.citation).toBe('compiler.rs:1284');
  });

  it('node.state-reference: a runtime built-in key needs no declaration (compiler.rs:1436)', () => {
    expect(ruleIds(withNode({ input: ['messages', 'session_id'] }))).not.toContain('node.state-reference');
  });

  it('node.state-reference: a HITL `edit_state_key` gets NO built-in escape (compiler.rs:1348)', () => {
    // `session_id` is a built-in (compiler.rs:1452), so it is legal as an
    // `input`/`output` key with no declaration — but `compiler.rs:1346`
    // calls `state.contains_key` bare for the edit key, so the SAME name is
    // refused there. Both halves are asserted, on one document.
    const hitl: YamlPipelineNode = { id: 'HITL_1', type: 'hitl', input: ['session_id'], routes: { approve: 'END' }, edit_state_key: 'session_id' };
    const issues = issuesForNode(collectGraphAdmissionIssues(baseDocument({ nodes: [hitl], entry_point: 'HITL_1' })), 'HITL_1');

    expect(issues).toHaveLength(1);
    expect(issues[0]?.field).toBe('edit_state_key');
    expect(issues[0]?.citation).toBe('compiler.rs:1348');
  });

  it('node.state-reference: a HITL `edit_state_key` set to "" is present-and-illegal, not absent (hitl.rs:161)', () => {
    // `Option<String>` — `Some("")` is Some, so `validate_routes` counts the
    // edit route as an action (`hitl.rs:477`) and then `hitl.rs:158-165`
    // refuses the node because `valid_output_key("")` is false
    // (`yaml.rs:372`). Treating `''` as "no key set" admitted the document.
    const hitl: YamlPipelineNode = { id: 'HITL_1', type: 'hitl', input: ['messages'], routes: { edit: 'HITL_1' }, edit_state_key: '' };
    const issues = issuesForNode(collectGraphAdmissionIssues(baseDocument({ nodes: [hitl], entry_point: 'HITL_1' })), 'HITL_1');

    expect(issues).toHaveLength(1);
    expect(issues[0]?.field).toBe('edit_state_key');
    expect(issues[0]?.subject).toBe('');
    expect(issues[0]?.citation).toBe('hitl.rs:161');
  });

  it('node.state-reference: an ABSENT HITL `edit_state_key` is legal — `None`, not `Some("")` (hitl.rs:158)', () => {
    const hitl: YamlPipelineNode = { id: 'HITL_1', type: 'hitl', input: ['messages'], routes: { approve: 'END' } };

    expect(collectGraphAdmissionIssues(baseDocument({ nodes: [hitl], entry_point: 'HITL_1' }))).toEqual([]);
  });

  it('node.state-reference: a Decision node validates `decisional_inputs`, not the `input` the compiler drops (decision.rs:97-101)', () => {
    // Both halves on the pair of documents that made the old rule wrong in
    // OPPOSITE directions. `routing_tests.rs:310-314` pins the precedence:
    // `input` beside a non-empty `decisional_inputs` never reaches
    // `input_keys()`, so validating it is a false refusal — and the key that
    // DOES reach it went unchecked.
    const stale: YamlPipelineNode = { id: 'Decision_1', type: 'decision', nodes: [], default_output: 'END', decisional_inputs: ['messages'], input: ['nowhere'] };
    expect(collectGraphAdmissionIssues(baseDocument({ nodes: [stale], entry_point: 'Decision_1' }))).toEqual([]);

    const live: YamlPipelineNode = { id: 'Decision_1', type: 'decision', nodes: [], default_output: 'END', decisional_inputs: ['nowhere'] };
    const issues = issuesForNode(collectGraphAdmissionIssues(baseDocument({ nodes: [live], entry_point: 'Decision_1' })), 'Decision_1');

    expect(issues).toHaveLength(1);
    expect(issues[0]?.field).toBe('decisional_inputs[0]');
    expect(issues[0]?.subject).toBe('nowhere');
    expect(issues[0]?.citation).toBe('compiler.rs:1284');
  });

  it('node.state-reference: an EMPTY `decisional_inputs` hands the read back to `input` (decision.rs:99)', () => {
    // `.filter(|values| !values.is_empty())` — an empty list loses to
    // `raw.input`, so `input` is the field the compiler reads and the field
    // the issue must name.
    const decision: YamlPipelineNode = { id: 'Decision_1', type: 'decision', nodes: [], default_output: 'END', decisional_inputs: [], input: ['nowhere'] };
    const issues = issuesForNode(collectGraphAdmissionIssues(baseDocument({ nodes: [decision], entry_point: 'Decision_1' })), 'Decision_1');

    expect(issues.map((issue) => issue.field)).toEqual(['input[0]']);
  });

  it('node.structured-output: a non-structured LLM node writes at most one data output (llm.rs:182)', () => {
    const document = withNode({ output: ['summary', 'draft'] }, { state: { input: 'str', messages: 'list', summary: 'str', draft: 'str' } });
    const issues = issuesForNode(collectGraphAdmissionIssues(document), 'LLM_1');

    expect(issues[0]?.rule).toBe('node.structured-output');
    expect(issues[0]?.message).toContain('found 2 (summary, draft)');
    expect(issues[0]?.citation).toBe('llm.rs:182');
  });

  it('node.structured-output: turning structured output on admits the same two outputs (llm.rs:176)', () => {
    const document = withNode(
      { output: ['summary', 'draft'], structured_output: true },
      { state: { input: 'str', messages: 'list', summary: 'str', draft: 'str' } },
    );

    expect(collectGraphAdmissionIssues(document)).toEqual([]);
  });

  it('node.structured-output: a structured LLM node must name a data output (llm.rs:186)', () => {
    const issues = issuesForNode(collectGraphAdmissionIssues(withNode({ output: ['messages'], structured_output: true })), 'LLM_1');

    expect(issues[0]?.rule).toBe('node.structured-output');
    expect(issues[0]?.citation).toBe('llm.rs:186');
  });

  it('node.structured-output: a structured Toolkit node must name a data output (direct_tool.rs:182)', () => {
    const toolkit: YamlPipelineNode = { id: 'Toolkit_1', type: 'toolkit', toolkit_name: 'github', tool: 'search', output: ['messages'], structured_output: true, transition: 'END' };
    const issues = issuesForNode(collectGraphAdmissionIssues(baseDocument({ nodes: [toolkit], entry_point: 'Toolkit_1' })), 'Toolkit_1');

    expect(issues[0]?.rule).toBe('node.structured-output');
    expect(issues[0]?.citation).toBe('direct_tool.rs:182');
  });
});

describe('collection surface', () => {
  it('reads an undefined document as an empty pipeline rather than throwing', () => {
    expect(collectGraphAdmissionIssues(undefined).map((issue) => issue.rule)).toEqual(['document.node-count', 'document.entry-point']);
  });

  it('splits issues by node so a panel can show only its own', () => {
    const document = baseDocument({
      nodes: [
        { ...ADMISSIBLE_LLM_NODE, output: ['nowhere'] },
        { id: 'LLM_2', type: 'llm', input: ['messages'], output: ['elsewhere'], transition: 'END' },
      ],
    });
    const issues = collectGraphAdmissionIssues(document);

    expect(issuesForNode(issues, 'LLM_1').map((issue) => issue.subject)).toEqual(['nowhere']);
    expect(issuesForNode(issues, 'LLM_2').map((issue) => issue.subject)).toEqual(['elsewhere']);
    expect(documentLevelIssues(issues)).toEqual([]);
  });
});
