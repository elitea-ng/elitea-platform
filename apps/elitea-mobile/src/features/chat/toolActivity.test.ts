import { golden, goldenData, frameOf, rmOf, type Worker } from '@/features/chat/__fixtures__/clientFrames';
import type { Frame } from '@/features/chat/frames';
import { MAX_TOOL_CHUNKS, MAX_TOOL_TEXT, markPausedSteps, pauseOf, prettyJson, type ToolStep } from '@/features/chat/toolActivity';
import { nodeOf } from '@/features/chat/__fixtures__/clientFrames';
import { INITIAL_TURN, isTurnTerminal, reduceTurn, type TurnState } from '@/features/chat/turnReducer';

/** Folds frames like the client: stop at the first terminal one. */
function play(frames: Frame[]): { state: TurnState; consumed: number } {
  let state: TurnState = INITIAL_TURN;
  let consumed = 0;
  for (const frame of frames) {
    state = reduceTurn(state, frame);
    consumed += 1;
    if (isTurnTerminal(frame)) break;
  }
  return { state, consumed };
}

const answer = (text: string): Frame => frameOf({ type: 'agent_response', content: text, response_metadata: { finish_reason: 'stop' } });

describe.each<Worker>(['python', 'rust'])('a tool call (%s worker golden frames)', (worker) => {
  it('runs, then completes with its inputs and output, and the turn goes on', () => {
    const afterStart = play([golden(worker, 'tool_start')]).state;
    expect(afterStart.phase).toBe('streaming');
    expect(afterStart.tools).toHaveLength(1);
    expect(afterStart.tools[0]).toMatchObject({ status: 'running', partial: false });
    expect(afterStart.status).toMatch(/^Using /);

    const { state } = play([golden(worker, 'tool_start'), golden(worker, 'tool_end'), answer('Done.')]);
    expect(state.phase).toBe('done');
    expect(state.status).toBeNull();
    expect(state.tools).toHaveLength(1);
    const step = state.tools[0];
    expect(step.status).toBe('done');
    // The python capture scrubs timestamps to "<timestamp>": unparsable, so absent.
    expect(step.startedAt).toEqual(worker === 'rust' ? expect.any(Number) : null);
    expect(step.endedAt).toEqual(worker === 'rust' ? expect.any(Number) : null);
    expect(step.inputs).toEqual(expect.stringContaining('{'));
    expect(step.output).toEqual(expect.any(String));
  });

  it('is never terminal, start or end', () => {
    expect(isTurnTerminal(golden(worker, 'tool_start'))).toBe(false);
    expect(isTurnTerminal(golden(worker, 'tool_end'))).toBe(false);
  });
});

describe('tool steps (python golden frames)', () => {
  it('reads the display name, toolkit and sub-agent', () => {
    const { state } = play([golden('python', 'tool_start'), golden('python', 'tool_end')]);
    expect(state.tools[0]).toMatchObject({
      id: 'run-1',
      name: 'list_issues',
      displayName: 'List issues',
      toolkit: 'github',
      parentAgent: 'researcher',
      inputs: '{\n  "state": "open"\n}',
      output: '{"items":2}',
    });
    expect(prettyJson(state.tools[0].output!)).toBe('{\n  "items": 2\n}');
  });

  it('marks a failed call with its error', () => {
    const { state } = play([golden('python', 'tool_error')]);
    expect(state.tools[0]).toMatchObject({ id: 'run-2', status: 'error', error: '"file not found"', partial: true });
  });

  it('builds the step from an end frame whose start was lost (partial)', () => {
    const { state } = play([golden('python', 'tool_end')]);
    expect(state.tools[0]).toMatchObject({ id: 'run-1', status: 'done', partial: true, output: '{"items":2}' });
  });

  it('assembles a chunked output in index order before the end frame', () => {
    const chunk = (index: number, content: string) =>
      golden('python', 'tool_output_chunk', (d) => {
        d.content = content;
        (rmOf(d).tool_output_chunk as Record<string, unknown>).index = index;
      });
    const { state } = play([chunk(2, 'C'), chunk(0, 'A'), chunk(3, 'D'), chunk(1, 'B'), golden('python', 'tool_end_chunked')]);
    expect(state.tools).toHaveLength(1);
    expect(state.tools[0]).toMatchObject({ id: 'run-3', status: 'done', output: 'ABCD', partial: true });
  });

  it('flags a chunked output with a missing chunk as partial and shows what came', () => {
    const chunk = (index: number, content: string) =>
      golden('python', 'tool_output_chunk', (d) => {
        d.content = content;
        (rmOf(d).tool_output_chunk as Record<string, unknown>).index = index;
      });
    const start = golden('python', 'tool_start', (d) => {
      rmOf(d).tool_run_id = 'run-3';
      rmOf(d).run_id = 'run-3';
    });
    const { state } = play([start, chunk(0, 'A'), chunk(1, 'B'), chunk(3, 'D'), golden('python', 'tool_end_chunked')]);
    expect(state.tools[0]).toMatchObject({ output: 'ABD', partial: true });
  });

  it('keeps at most 8 KiB of inputs and output in memory', () => {
    const big = 'x'.repeat(MAX_TOOL_TEXT * 3);
    const end = golden('python', 'tool_end', (d) => {
      rmOf(d).tool_output = big;
      rmOf(d).tool_inputs = { blob: big };
    });
    const { state } = play([end]);
    expect(state.tools[0].output).toHaveLength(MAX_TOOL_TEXT);
    expect(state.tools[0].outputTruncated).toBe(true);
    expect(state.tools[0].inputs).toHaveLength(MAX_TOOL_TEXT);
    expect(state.tools[0].inputsTruncated).toBe(true);
  });

  it('takes no chunk total beyond MAX_TOOL_CHUNKS (no loop or map sized by the server)', () => {
    const huge = golden('python', 'tool_output_chunk', (d) => {
      (rmOf(d).tool_output_chunk as Record<string, unknown>).total = MAX_TOOL_CHUNKS + 1;
    });
    const after = play([huge]).state.tools;
    expect(after[0]).toMatchObject({ id: 'run-3', partial: true, chunks: null });
    // An end frame declaring 2^40 chunks and no output settles at once.
    const end = golden('python', 'tool_end_chunked', (d) => {
      (rmOf(d).tool_output_chunks as Record<string, unknown>).total = 2 ** 40;
    });
    expect(play([end]).state.tools[0]).toMatchObject({ status: 'done', output: '', partial: true });
  });

  it('stops recording chunks once the kept output reaches the cap', () => {
    const chunk = (index: number, content: string) =>
      golden('python', 'tool_output_chunk', (d) => {
        d.content = content;
        const meta = rmOf(d).tool_output_chunk as Record<string, unknown>;
        meta.index = index;
        meta.total = 2000;
      });
    const frames = [chunk(0, 'x'.repeat(MAX_TOOL_TEXT + 5)), ...Array.from({ length: 1999 }, (_, i) => chunk(i + 1, 'y'))];
    const step = play(frames).state.tools[0];
    expect(Object.keys(step.chunks?.received ?? {})).toHaveLength(1);
    expect(step.partial).toBe(true);
  });

  it('ignores a chunk frame without a usable key, index or total', () => {
    const bad = golden('python', 'tool_output_chunk', (d) => {
      (rmOf(d).tool_output_chunk as Record<string, unknown>).index = 9;
    });
    expect(play([bad]).state.tools).toEqual([]);
  });
});

describe('HITL pauses', () => {
  it('single (python): terminal, one tool-call interrupt with its args', () => {
    const { state, consumed } = play([golden('python', 'tool_start'), golden('python', 'hitl_interrupt'), answer('late')]);
    expect(consumed).toBe(2);
    expect(state.phase).toBe('paused');
    expect(state.pause).toMatchObject({ kind: 'hitl', shape: 'single', threadId: 'thread-1', messageId: 'message-1' });
    const pause = state.pause!;
    if (pause.kind !== 'hitl') throw new Error('not a hitl pause');
    expect(pause.interrupts).toEqual([
      expect.objectContaining({
        interruptId: 'interrupt-1',
        toolCallId: 'call-1',
        availableActions: ['approve', 'reject', 'edit'],
        guardrailType: 'sensitive_tool',
        toolName: 'delete_branch',
        toolkitName: 'github',
        toolArgs: { branch: 'old' },
        message: 'Approve?',
      }),
    ]);
  });

  it('single (rust pipeline node): terminal, no tool call id', () => {
    const { state } = play([golden('rust', 'hitl_interrupt')]);
    expect(state.phase).toBe('paused');
    const pause = state.pause!;
    if (pause.kind !== 'hitl') throw new Error('not a hitl pause');
    expect(pause.shape).toBe('single');
    expect(pause.interrupts[0]).toMatchObject({
      interruptId: 'hitl_g1:Nlk3CWHTb-6EP_u82zbXYRxxMT0C8duRRCC4vS8l6PI',
      toolCallId: null,
      guardrailType: 'pipeline_hitl',
      message: 'Review the generated answer.',
    });
  });

  it('parallel: several interrupts in one frame, terminal, one entry each', () => {
    const frame = golden('python', 'hitl_interrupt', (d) => {
      const rm = rmOf(d);
      const first = (rm.hitl_interrupts as Record<string, unknown>[])[0];
      rm.hitl_interrupts = [
        { ...first, parent_agent_name: 'a' },
        { ...first, interrupt_id: 'interrupt-2', tool_call_id: 'call-2', parent_agent_name: 'b', tool_args: { branch: 'new' } },
      ];
    });
    expect(isTurnTerminal(frame)).toBe(true);
    const pause = play([frame]).state.pause!;
    if (pause.kind !== 'hitl') throw new Error('not a hitl pause');
    expect(pause.shape).toBe('parallel');
    expect(pause.interrupts.map((i) => [i.interruptId, i.toolCallId, i.parentAgentName])).toEqual([
      ['interrupt-1', 'call-1', 'a'],
      ['interrupt-2', 'call-2', 'b'],
    ]);
  });

  it('fan-out children: not terminal, collected while the siblings stream', () => {
    const child = (id: string, thread: string) =>
      golden('python', 'hitl_interrupt', (d) => {
        const rm = rmOf(d);
        rm.metadata = { parent_agent_name: 'orchestrator', child_thread_id: thread };
        const entry = { ...(rm.hitl_interrupt as Record<string, unknown>), interrupt_id: id };
        rm.hitl_interrupt = entry;
        rm.hitl_interrupts = [entry];
      });
    const a = child('interrupt-a', 'child-1');
    expect(isTurnTerminal(a)).toBe(false);
    const { state, consumed } = play([a, child('interrupt-b', 'child-2'), child('interrupt-a', 'child-1'), answer('Siblings done.')]);
    expect(consumed).toBe(4);
    expect(state.phase).toBe('done');
    const pause = state.pause!;
    if (pause.kind !== 'hitl') throw new Error('not a hitl pause');
    expect(pause.shape).toBe('fanout');
    expect(pause.interrupts.map((i) => [i.interruptId, i.childThreadId, i.parentAgentName])).toEqual([
      ['interrupt-a', 'child-1', 'orchestrator'],
      ['interrupt-b', 'child-2', 'orchestrator'],
    ]);
  });
});

describe('MCP authorization', () => {
  it('progress (python): not terminal, the tool waits for sign-in', () => {
    const progress = golden('python', 'mcp_authorization_progress');
    expect(isTurnTerminal(progress)).toBe(false);
    const { state } = play([progress]);
    expect(state.phase).toBe('streaming');
    expect(state.pause).toBeNull();
    expect(state.tools[0]).toMatchObject({ id: 'auth-1', name: 'search', toolkit: 'example', status: 'needs_auth' });
  });

  it('terminal (python): pauses with each request', () => {
    const { state, consumed } = play([golden('python', 'mcp_authorization_progress'), golden('python', 'mcp_authorization_terminal'), answer('late')]);
    expect(consumed).toBe(2);
    expect(state.phase).toBe('paused');
    expect(state.pause).toEqual({
      kind: 'auth',
      threadId: 'thread-1',
      messageId: 'message-1',
      requests: [
        {
          requestId: 'auth-1',
          toolCallId: null,
          serverUrl: 'https://mcp.example.test/mcp',
          toolName: 'search',
          toolkitName: 'example',
          toolkitType: 'mcp',
          resourceName: 'Example',
        },
      ],
    });
  });

  it('the rust pipeline frame without authorization_requests is progress (the documented rule)', () => {
    const frame = golden('rust', 'mcp_authorization');
    expect(isTurnTerminal(frame)).toBe(false);
    expect(play([frame]).state).toMatchObject({ phase: 'streaming', pause: null });
  });
});

describe('output limit', () => {
  it('records the Continue offer and still settles on the final answer (rust golden frame)', () => {
    const offer = golden('rust', 'requires_confirmation');
    expect(isTurnTerminal(offer)).toBe(false);
    const { state } = play([offer, answer('Partial answer.')]);
    expect(state.phase).toBe('done');
    expect(state.pause).toEqual({ kind: 'output_limit', threadId: 'thread-1', messageId: 'message-1', action: 'Continue' });
  });
});

describe('golden frames are complete', () => {
  it('has every catalogued frame kind the app reads', () => {
    expect(Object.keys(goldenData('python', 'tool_start'))).toEqual(expect.arrayContaining(['type', 'message_id', 'response_metadata']));
    for (const name of ['tool_start', 'tool_end', 'tool_end_chunked', 'tool_error', 'tool_output_chunk', 'hitl_interrupt', 'mcp_authorization_progress', 'mcp_authorization_terminal']) {
      expect(goldenData('python', name).type).toEqual(expect.any(String));
    }
    for (const name of ['tool_start', 'tool_end', 'hitl_interrupt', 'mcp_authorization', 'requires_confirmation']) {
      expect(goldenData('rust', name).type).toEqual(expect.any(String));
    }
  });
});

describe('a tool paused for approval (captured, python worker)', () => {
  // eslint-disable-next-line @typescript-eslint/no-require-imports
  const captured = require('./__fixtures__/captured/python-sensitive-tool-pause.json') as { frames: Record<string, unknown>[] };
  const frames = captured.frames.map((f) => frameOf(f));

  it('is waiting, not failed: the worker reports the interrupt as a tool error first', () => {
    expect(frames.map((f) => (f.kind === 'node' ? f.node.type : f.kind))).toEqual([
      'agent_tool_start',
      'agent_tool_error',
      'agent_hitl_interrupt',
    ]);
    const { state } = play(frames);
    expect(state.phase).toBe('paused');
    expect(state.tools).toHaveLength(1);
    expect(state.tools[0]).toMatchObject({ name: 'mock_tool_status', status: 'waiting', error: null });
  });

  it('keeps a real tool error an error', () => {
    const real = captured.frames[1];
    const failed = frameOf({
      ...real,
      content: 'HTTP 500 from the tool',
      response_metadata: { ...(real.response_metadata as Record<string, unknown>), error: 'HTTP 500 from the tool' },
    });
    const { state } = play([frames[0], failed]);
    expect(state.tools[0]).toMatchObject({ status: 'error', error: 'HTTP 500 from the tool' });
  });
});

describe('persisted steps of a paused answer', () => {
  // eslint-disable-next-line @typescript-eslint/no-require-imports
  const captured = require('./__fixtures__/captured/python-sensitive-tool-pause.json') as { frames: Record<string, unknown>[] };
  const pause = pauseOf(nodeOf(captured.frames[2]))!;
  const persisted = (id: string, name: string, status: ToolStep['status']): ToolStep => ({
    id, name, displayName: null, toolkit: 'MockTools', parentAgent: null, status,
    startedAt: null, endedAt: null, inputs: null, inputsTruncated: false, output: null, outputTruncated: false,
    partial: false, error: null, chunks: null,
  });

  it('shows the call the pause waits on as waiting (the server stores it as an error)', () => {
    const steps = [persisted('t1', 'mock_tool_create_item', 'error'), persisted('t2', 'mock_tool_status', 'error')];
    expect(markPausedSteps(steps, pause).map((s) => s.status)).toEqual(['error', 'waiting']);
    // Only the latest failed call of that tool, and nothing without a hitl pause.
    const twice = [persisted('a', 'mock_tool_status', 'error'), persisted('b', 'mock_tool_status', 'error')];
    expect(markPausedSteps(twice, pause).map((s) => s.status)).toEqual(['error', 'waiting']);
    expect(markPausedSteps(steps, null)).toBe(steps);
    expect(markPausedSteps([persisted('c', 'mock_tool_status', 'done')], pause)[0].status).toBe('done');
  });
});
