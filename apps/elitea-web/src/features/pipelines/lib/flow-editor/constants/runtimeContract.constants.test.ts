import { describe, expect, it } from 'vitest';

import {
  CompilerAdmittedNodeTypes,
  MAX_NODE_ID_BYTES,
  NODE_ID_PATTERN,
  NODE_ID_WORD_SEPARATOR,
  ReservedStateKeys,
  isCompilerAdmittedNodeType,
  isReservedStateKey,
} from './runtimeContract.constants';
import { PipelineNodeTypes } from './flowEditor.constants';

describe('NODE_ID_PATTERN mirrors valid_graph_id (worker yaml.rs:362)', () => {
  it('admits ASCII alphanumerics plus _ - . : and nothing else', () => {
    for (const legal of ['Agent_1', 'a-b', 'a.b', 'a:b', 'END', 'Z9']) {
      expect(NODE_ID_PATTERN.test(legal)).toBe(true);
    }
    for (const illegal of ['Agent 1', '', 'a/b', 'a#b', 'a~b', 'ág']) {
      expect(NODE_ID_PATTERN.test(illegal)).toBe(false);
    }
  });

  it('the word separator it mints with is itself in the admitted set', () => {
    expect(NODE_ID_PATTERN.test(NODE_ID_WORD_SEPARATOR)).toBe(true);
    expect(NODE_ID_WORD_SEPARATOR).not.toBe(' ');
  });

  it('carries the runtime byte ceiling (yaml.rs:10)', () => {
    expect(MAX_NODE_ID_BYTES).toBe(128);
  });

  it('the END the seeded route defaults use is the literal the runtime compares against', () => {
    // `compiler.rs:484` / `router.rs:331` / `hitl.rs:464` all compare
    // `target != "END"` byte for byte, and `nodeDefaults.constants.ts` seeds
    // every route default from `PipelineNodeTypes.End`.
    expect(PipelineNodeTypes.End).toBe('END');
    expect(NODE_ID_PATTERN.test('END')).toBe(true);
  });
});

describe('CompilerAdmittedNodeTypes mirrors parse_pipeline_node (worker compiler.rs:1236)', () => {
  it('holds exactly the eleven parsed types the compiler has an arm for', () => {
    expect([...CompilerAdmittedNodeTypes].sort()).toEqual(
      ['agent', 'code', 'decision', 'hitl', 'llm', 'mcp', 'parallel', 'printer', 'router', 'state_modifier', 'toolkit'].sort(),
    );
  });

  it('admits sandbox Code nodes and excludes unsupported custom nodes', () => {
    expect(isCompilerAdmittedNodeType(PipelineNodeTypes.Code)).toBe(true);
    expect(isCompilerAdmittedNodeType(PipelineNodeTypes.Custom)).toBe(false);
  });

  it('excludes every deprecated type the runtime dropped', () => {
    for (const type of [
      PipelineNodeTypes.Tool,
      PipelineNodeTypes.Function,
      PipelineNodeTypes.Condition,
      PipelineNodeTypes.Pipeline,
      PipelineNodeTypes.Loop,
      PipelineNodeTypes.LoopFromTool,
    ]) {
      expect(isCompilerAdmittedNodeType(type)).toBe(false);
    }
  });

  it('names no type twice', () => {
    expect(new Set(CompilerAdmittedNodeTypes).size).toBe(CompilerAdmittedNodeTypes.length);
  });
});

describe('ReservedStateKeys mirrors reserved_user_state_key (worker compiler.rs:1456)', () => {
  it('holds the 25 recorded reserved keys, each citing the compiler line that reserves it', () => {
    expect(ReservedStateKeys).toHaveLength(25);
    for (const entry of ReservedStateKeys) {
      expect(entry.key.length).toBeGreaterThan(0);
      expect(entry.citation).toMatch(/^compiler\.rs:\d+/);
      expect(entry.reason.length).toBeGreaterThan(0);
    }
  });

  it('reserves the four private resume/scope channels by their literal value', () => {
    for (const key of [
      '__elitea_hitl_resume_v1',
      '__elitea_tool_resume_v1',
      '__elitea_llm_tool_resume_v1',
      '__elitea_pipeline_node_event_scope_v1',
    ]) {
      expect(isReservedStateKey(key)).toBe(true);
    }
  });

  it('reserves the ordinary-looking names that would otherwise pass the character rule', () => {
    for (const key of ['output', 'result', 'session_id', 'thread_id', 'chat_history', 'execution_finished']) {
      expect(isReservedStateKey(key)).toBe(true);
    }
  });

  it('does NOT reserve input/messages — builtin_state_key (compiler.rs:1436) is a wider, different set', () => {
    // These two are exactly the `DefaultState` keys the editor seeds into
    // every pipeline. Reserving them here would make every new pipeline
    // unauthorable.
    expect(isReservedStateKey('input')).toBe(false);
    expect(isReservedStateKey('messages')).toBe(false);
  });

  it('names no key twice', () => {
    expect(new Set(ReservedStateKeys.map(entry => entry.key)).size).toBe(ReservedStateKeys.length);
  });
});
