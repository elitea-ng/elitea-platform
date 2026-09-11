/**
 * The frames here are not invented: they are the shape a live standalone stack
 * emits, captured from the SSE stream while the backend chat smoke ran
 * (`deploy/scripts/chat-smoke.py`). The happy-path test replays that exact
 * sequence, which is why it is evidence that the reducer renders a real turn
 * rather than a turn someone imagined.
 */
import { describe, expect, it } from 'vitest';

import { applyChatStreamFrame, mcpSessionFromFrame, type ChatStreamContext, type ToolAction } from './chatStreamReducer';
import { HANDLED_STREAM_TYPES, SocketMessageType, isChatStreamFrame } from './chatStreamFrame';
import type { ChatMessage } from './convertMessagesToChatHistory';

const MESSAGE_ID = '63c6d989-2860-5d68-9e3e-3587c63350d3';
const QUESTION_ID = '11111111-2222-3333-4444-555555555555';
const CONTEXT: ChatStreamContext = { name: 'Agent', now: () => '2026-08-13T00:00:00.000Z' };

/** The assistant placeholder the send path appends before the stream opens. */
function pendingAssistant(): ChatMessage {
  return {
    id: MESSAGE_ID,
    role: 'assistant',
    name: 'Agent',
    content: '',
    createdAt: '2026-08-13T00:00:00.000Z',
    questionId: QUESTION_ID,
    isStreaming: true,
    isLoading: true,
  };
}

function frame(type: string, extra: Record<string, unknown> = {}) {
  return { type, message_id: MESSAGE_ID, question_id: QUESTION_ID, stream_id: 's-1', ...extra };
}

describe('applyChatStreamFrame', () => {
  it('renders a real turn from the sequence a live stack emits', () => {
    // Recorded order: agent_start, agent_on_transitional_edge, agent_llm_start,
    // agent_llm_chunk ×4, agent_llm_end, agent_response, pipeline_finish.
    const sequence = [
      frame(SocketMessageType.AgentStart),
      frame(SocketMessageType.AgentOnTransitionalEdge),
      frame(SocketMessageType.AgentLlmStart),
      frame(SocketMessageType.AgentLlmChunk, { content: 'MOCK: ' }),
      frame(SocketMessageType.AgentLlmChunk, { content: 'chat ' }),
      frame(SocketMessageType.AgentLlmChunk, { content: 'smoke ' }),
      frame(SocketMessageType.AgentLlmChunk, { content: '1786639081 ' }),
      frame(SocketMessageType.AgentLlmEnd),
      frame(SocketMessageType.PipelineFinish),
    ];

    const result = sequence.reduce(
      (history, next) => applyChatStreamFrame(history, next, CONTEXT),
      [pendingAssistant()] as readonly ChatMessage[],
    );

    expect(result).toHaveLength(1);
    expect(result[0]?.content).toBe('MOCK: chat smoke 1786639081 ');
    expect(result[0]?.isStreaming).toBe(false);
    expect(result[0]?.isLoading).toBe(false);
  });

  it('finishes the turn on a response carrying finish_reason, and keeps the thread id', () => {
    const history = applyChatStreamFrame(
      [{ ...pendingAssistant(), content: 'partial' }],
      frame(SocketMessageType.AgentResponse, {
        content: '',
        response_metadata: { finish_reason: 'stop', metadata: { thread_id: 'thread-9' } },
      }),
      CONTEXT,
    );

    expect(history[0]?.isStreaming).toBe(false);
    expect(history[0]?.threadId).toBe('thread-9');
  });

  it('does NOT finish the turn on an intermediate response with no finish_reason', () => {
    // A mid-turn agent_response is ordinary in a pipeline; treating it as
    // terminal would stop the spinner while tokens are still arriving.
    const history = applyChatStreamFrame(
      [pendingAssistant()],
      frame(SocketMessageType.AgentResponse, { content: 'step one' }),
      CONTEXT,
    );

    expect(history[0]?.isStreaming).toBe(true);
    expect(history[0]?.content).toContain('step one');
  });

  it('resets content when a turn restarts, so a regenerate does not append', () => {
    const history = applyChatStreamFrame(
      [{ ...pendingAssistant(), content: 'previous answer' }],
      frame(SocketMessageType.AgentStart),
      CONTEXT,
    );

    expect(history[0]?.content).toBe('');
  });

  it('surfaces a failure without discarding what already streamed', () => {
    const history = applyChatStreamFrame(
      [{ ...pendingAssistant(), content: 'got this far' }],
      frame(SocketMessageType.AgentException, { content: 'boom' }),
      CONTEXT,
    );

    expect(history[0]?.exception).toBe('boom');
    expect(history[0]?.isStreaming).toBe(false);
    // The partial answer is what the user watched arrive; hiding it would erase
    // the only evidence of how far the run got.
    expect(history[0]?.content).toBe('got this far');
  });

  it('resolves a frame by question id when the assistant message has no id yet', () => {
    const pending: ChatMessage = { ...pendingAssistant(), id: 'local-placeholder' };
    const history = applyChatStreamFrame(
      [pending],
      { type: SocketMessageType.AgentLlmChunk, question_id: QUESTION_ID, content: 'hi' },
      CONTEXT,
    );

    expect(history).toHaveLength(1);
    expect(history[0]?.content).toBe('hi');
  });

  it('creates the assistant message when a chunk arrives for one it has never seen', () => {
    const history = applyChatStreamFrame([], frame(SocketMessageType.AgentLlmChunk, { content: 'hello' }), CONTEXT);

    expect(history).toHaveLength(1);
    expect(history[0]?.role).toBe('assistant');
    expect(history[0]?.content).toBe('hello');
  });

  it('leaves state untouched — by reference — for a type this slice has not ported', () => {
    // The reference check is the assertion that matters: an unported frame must
    // be inert, never a partial write, and must not re-render the list.
    const before: readonly ChatMessage[] = [pendingAssistant()];
    const after = applyChatStreamFrame(before, frame(SocketMessageType.SwarmChildMessage), CONTEXT);

    expect(after).toBe(before);
  });

  it('leaves state untouched for every graph frame, and for freeform', () => {
    // These are state-inert BY DESIGN, not unported: the baseline's five graph
    // cases contain nothing but the forward call, and its freeform case is a
    // bare `break`. A caller forwards them via shouldForwardAgentEvent; the
    // reducer must not invent a state change the baseline never made.
    const before: readonly ChatMessage[] = [pendingAssistant()];
    for (const type of [
      SocketMessageType.AgentOnFunctionToolNode,
      SocketMessageType.AgentOnToolNode,
      SocketMessageType.AgentOnTransitionalEdge,
      SocketMessageType.AgentOnConditionalEdge,
      SocketMessageType.AgentOnDecisionEdge,
      'agent_on_loop_node',
      SocketMessageType.Freeform,
    ]) {
      const after = applyChatStreamFrame(before, frame(type, { content: 'ignored' }), CONTEXT);
      expect(after, `${type} changed message state`).toBe(before);
    }
  });

  it('ignores a frame with no type at all', () => {
    const before: readonly ChatMessage[] = [pendingAssistant()];
    expect(applyChatStreamFrame(before, { message_id: MESSAGE_ID }, CONTEXT)).toBe(before);
  });
});

describe('the ported boundary is explicit', () => {
  it('every handled type is reduced, and every unhandled one is inert', () => {
    // The fixture satisfies EVERY handled case's preconditions — an
    // already-started tool action for the end/error cases, a processing summary
    // for chat_predict_summary_finished (which closes by type rather than by
    // run id), and a `uuid` for chat_user_message (whose id is uuid, not
    // message_id) — so a "handled type changed nothing" failure means the case
    // is genuinely unreachable rather than under-supplied by the test.
    const before: readonly ChatMessage[] = [
      {
        ...pendingAssistant(),
        toolActions: [
          { id: 'run-x', type: 'tool', status: 'processing' } as ToolAction,
          { id: 'run-sum', type: 'summary', status: 'processing' } as ToolAction,
        ],
      },
    ];

    for (const type of Object.values(SocketMessageType)) {
      const next = applyChatStreamFrame(
        before,
        frame(type, { content: 'x', references: [], uuid: 'echo-uuid', response_metadata: { tool_run_id: 'run-x' } }),
        CONTEXT,
      );
      if (HANDLED_STREAM_TYPES.has(type)) {
        expect(next, `${type} is listed as handled but changed nothing`).not.toBe(before);
      } else {
        expect(next, `${type} is not ported but mutated state`).toBe(before);
      }
    }
  });
});

describe('isChatStreamFrame', () => {
  it('accepts a frame with a type and rejects anything else', () => {
    expect(isChatStreamFrame({ type: 'agent_llm_chunk' })).toBe(true);
    expect(isChatStreamFrame({ message_id: 'x' })).toBe(false);
    expect(isChatStreamFrame(null)).toBe(false);
    expect(isChatStreamFrame('agent_llm_chunk')).toBe(false);
  });
});

describe('the tool lifecycle', () => {
  const RUN_ID = 'run-1';

  function toolFrame(type: string, responseMetadata: Record<string, unknown>, extra: Record<string, unknown> = {}) {
    return frame(type, { response_metadata: { tool_run_id: RUN_ID, ...responseMetadata }, ...extra });
  }

  function withStartedTool(): readonly ChatMessage[] {
    return applyChatStreamFrame(
      [pendingAssistant()],
      toolFrame(SocketMessageType.AgentToolStart, {
        tool_name: 'jira___create_issue',
        tool_inputs: { summary: 'hi' },
        metadata: { thread_id: 'thread-3' },
      }),
      CONTEXT,
    );
  }

  it('creates a processing tool action and captures the thread id', () => {
    const history = withStartedTool();
    const action = (history[0]?.toolActions ?? [])[0] as ToolAction | undefined;

    expect(action?.['id']).toBe(RUN_ID);
    expect(action?.['status']).toBe('processing');
    expect(action?.['type']).toBe('tool');
    expect(action?.['toolInputs']).toEqual({ summary: 'hi' });
    expect(history[0]?.threadId).toBe('thread-3');
  });

  it('derives the toolkit from the legacy toolkit___tool name when metadata omits it', () => {
    const action = (withStartedTool()[0]?.toolActions ?? [])[0] as ToolAction;
    expect((action['toolMeta'] as Record<string, unknown>)['toolkit_name']).toBe('jira');
  });

  it('recovers a toolkit name from either description shape', () => {
    for (const [description, expected] of [
      ['[Toolkit: vectorstore]\nDoes things', 'vectorstore'],
      ['Does things\nToolkit: confluence', 'confluence'],
    ] as const) {
      const history = applyChatStreamFrame(
        [pendingAssistant()],
        toolFrame(SocketMessageType.AgentToolStart, { tool_name: 'search', tool_meta: { description } }),
        CONTEXT,
      );
      const action = (history[0]?.toolActions ?? [])[0] as ToolAction;
      expect((action['toolMeta'] as Record<string, unknown>)['toolkit_name']).toBe(expected);
    }
  });

  it('prefers the real tool name over a lazy-loading wrapper class', () => {
    // Without this the chip reads "LazyLoading" instead of the invoked tool.
    const history = applyChatStreamFrame(
      [pendingAssistant()],
      toolFrame(SocketMessageType.AgentToolStart, {
        tool_name: 'LazyLoading',
        tool_meta: { name: 'get_plan_status', metadata: { original_name: 'get_plan_status' } },
      }),
      CONTEXT,
    );
    const action = (history[0]?.toolActions ?? [])[0] as ToolAction;

    expect(action['name']).toBe('get_plan_status');
    expect(action['original_name']).toBe('get_plan_status');
  });

  it('does not duplicate an action when the same run id starts twice', () => {
    const once = withStartedTool();
    const twice = applyChatStreamFrame(once, toolFrame(SocketMessageType.AgentToolStart, {}), CONTEXT);

    expect(twice[0]?.toolActions).toHaveLength(1);
  });

  it('keeps chunked output running until its final frame and ignores replay', () => {
    let history = withStartedTool();
    const initialStatus = (history[0]?.toolActions?.[0] as ToolAction | undefined)?.status;
    const first = toolFrame(SocketMessageType.AgentToolEnd, { tool_output: '{"ok":', tool_output_chunk_v1: { offset_bytes: 0, total_bytes: 11, sha256: 'a'.repeat(64), final: false } });
    history = applyChatStreamFrame(history, first, CONTEXT);
    history = applyChatStreamFrame(history, first, CONTEXT);
    expect((history[0]?.toolActions?.[0] as ToolAction | undefined)?.status).toBe(initialStatus);
    history = applyChatStreamFrame(history, toolFrame(SocketMessageType.AgentToolEnd, { tool_output: 'true}', tool_output_chunk_v1: { offset_bytes: 6, total_bytes: 11, sha256: 'a'.repeat(64), final: true } }), CONTEXT);
    const action = history[0]?.toolActions?.[0] as ToolAction;
    expect(action['toolOutputs']).toBe('{"ok":true}');
    expect(action.status).toBe('complete');
  });

  it('assembles the final error fragment and keeps the tool failed on replay', () => {
    let history = withStartedTool();
    const output = JSON.stringify({ error: 'large failure' });
    const split = 9;
    const metadata = { total_bytes: output.length, sha256: 'a'.repeat(64) };
    history = applyChatStreamFrame(history, toolFrame(SocketMessageType.AgentToolEnd, {
      tool_output: output.slice(0, split),
      tool_output_chunk_v1: { ...metadata, offset_bytes: 0, final: false },
    }), CONTEXT);
    const last = toolFrame(SocketMessageType.AgentToolError, {
      tool_output: output.slice(split), finish_reason: 'error',
      tool_output_chunk_v1: { ...metadata, offset_bytes: split, final: true },
    });
    history = applyChatStreamFrame(history, last, CONTEXT);
    history = applyChatStreamFrame(history, last, CONTEXT);
    const action = history[0]?.toolActions?.[0] as ToolAction;
    expect(action['toolOutputs']).toBe(output);
    expect(action.status).toBe('error');
    expect(action['isError']).toBe(true);
  });

  it('accumulates string outputs across end frames rather than replacing them', () => {
    // A tool can report progressively; replacing would keep only the last frame.
    let history = withStartedTool();
    history = applyChatStreamFrame(history, toolFrame(SocketMessageType.AgentToolEnd, { tool_output: 'part one ' }), CONTEXT);
    history = applyChatStreamFrame(history, toolFrame(SocketMessageType.AgentToolEnd, { tool_output: 'part two' }), CONTEXT);
    const action = (history[0]?.toolActions ?? [])[0] as ToolAction;

    expect(String(action['toolOutputs'])).toContain('part one');
    expect(String(action['toolOutputs'])).toContain('part two');
    expect(action['status']).toBe('complete');
  });

  it('merges object outputs', () => {
    let history = withStartedTool();
    history = applyChatStreamFrame(history, toolFrame(SocketMessageType.AgentToolEnd, { tool_output: { a: 1 } }), CONTEXT);
    history = applyChatStreamFrame(history, toolFrame(SocketMessageType.AgentToolEnd, { tool_output: { b: 2 } }), CONTEXT);
    const action = (history[0]?.toolActions ?? [])[0] as ToolAction;

    expect(action['toolOutputs']).toEqual({ a: 1, b: 2 });
  });

  it('leaves an approval-gated action awaiting approval when its wrapper ends', () => {
    // The wrapper ending is not the user answering.
    const started = withStartedTool();
    const gated = started.map((message) => ({
      ...message,
      toolActions: (message.toolActions ?? []).map((action) => ({ ...action, status: 'action_required' })),
    })) as readonly ChatMessage[];
    const ended = applyChatStreamFrame(gated, toolFrame(SocketMessageType.AgentToolEnd, {}), CONTEXT);
    const action = (ended[0]?.toolActions ?? [])[0] as ToolAction;

    expect(action['status']).toBe('action_required');
  });

  it('marks a failed tool without touching the others', () => {
    let history = withStartedTool();
    history = applyChatStreamFrame(
      history,
      frame(SocketMessageType.AgentToolStart, { response_metadata: { tool_run_id: 'run-2', tool_name: 'other' } }),
      CONTEXT,
    );
    history = applyChatStreamFrame(history, toolFrame(SocketMessageType.AgentToolError, {}, { content: 'boom' }), CONTEXT);
    const actions = (history[0]?.toolActions ?? []) as readonly ToolAction[];

    expect(actions).toHaveLength(2);
    expect(actions[0]?.['status']).toBe('error');
    expect(actions[0]?.['isError']).toBe(true);
    expect(actions[1]?.['status']).toBe('processing');
  });

  it('ignores an end or error for a run id it never saw start', () => {
    const before = withStartedTool();
    expect(applyChatStreamFrame(before, frame(SocketMessageType.AgentToolEnd, { response_metadata: { tool_run_id: 'ghost' } }), CONTEXT)).toBe(before);
    expect(applyChatStreamFrame(before, frame(SocketMessageType.AgentToolError, { response_metadata: { tool_run_id: 'ghost' } }), CONTEXT)).toBe(before);
  });
});

describe('mcpSessionFromFrame', () => {
  it('surfaces the session a caller must persist, since the reducer cannot', () => {
    expect(
      mcpSessionFromFrame({
        type: SocketMessageType.AgentToolEnd,
        response_metadata: { metadata: { mcp_session_id: 's-1', mcp_server_url: 'https://mcp.example' } },
      }),
    ).toEqual({ sessionId: 's-1', serverUrl: 'https://mcp.example' });
  });

  it('returns nothing when either half is missing', () => {
    expect(mcpSessionFromFrame({ type: 'x', response_metadata: { metadata: { mcp_session_id: 's-1' } } })).toBeUndefined();
    expect(mcpSessionFromFrame({ type: 'x' })).toBeUndefined();
  });
});

describe('thinking steps', () => {
  const RUN = 'run-think';

  function withAction(overrides: Partial<ToolAction> = {}): readonly ChatMessage[] {
    return [
      {
        ...pendingAssistant(),
        toolActions: [{ id: RUN, type: 'tool', status: 'processing', ...overrides } as ToolAction],
      },
    ];
  }

  function actionsOf(history: readonly ChatMessage[]): readonly ToolAction[] {
    return (history[0]?.toolActions ?? []) as readonly ToolAction[];
  }

  it('creates a placeholder when a step arrives before its tool started', () => {
    // Steps can precede agent_tool_start; dropping them loses the progress the
    // user is waiting to see.
    const history = applyChatStreamFrame(
      [pendingAssistant()],
      frame(SocketMessageType.AgentThinkingStep, {
        response_metadata: { tool_run_id: RUN, message: 'searching…' },
      }),
      CONTEXT,
    );
    const action = actionsOf(history)[0];

    expect(action?.id).toBe(`thinking_step_${RUN}`);
    expect(action?.['message']).toBe('searching…');
    expect(action?.status).toBe('processing');
  });

  it('gives id-less steps DISTINCT placeholders', () => {
    // The baseline's `'thinking_step_' + id || '' + v4()` parses as
    // `('thinking_step_' + id) || …`, always truthy, so every id-less step
    // collided on "thinking_step_undefined". Two steps must not become one.
    let history = applyChatStreamFrame([pendingAssistant()], frame(SocketMessageType.AgentThinkingStep, {}), CONTEXT);
    history = applyChatStreamFrame(history, frame(SocketMessageType.AgentThinkingStep, {}), CONTEXT);
    const ids = actionsOf(history).map((action) => action.id);

    expect(ids).toHaveLength(2);
    expect(new Set(ids).size).toBe(2);
  });

  it('updates an existing action instead of adding a second one', () => {
    const history = applyChatStreamFrame(
      withAction(),
      frame(SocketMessageType.AgentThinkingStep, { response_metadata: { tool_run_id: RUN, message: 'step two' } }),
      CONTEXT,
    );

    expect(actionsOf(history)).toHaveLength(1);
    expect(actionsOf(history)[0]?.['message']).toBe('step two');
  });

  it('honours an explicit markdown:false, which the baseline could not', () => {
    // `markdown || true` can only ever be true.
    const history = applyChatStreamFrame(
      withAction(),
      frame(SocketMessageType.AgentThinkingStep, {
        response_metadata: { tool_run_id: RUN, markdown: false, message: 'x' },
      }),
      CONTEXT,
    );

    expect(actionsOf(history)[0]?.['markdown']).toBe(false);
  });

  it('applies progress text on an update', () => {
    const history = applyChatStreamFrame(
      withAction(),
      frame(SocketMessageType.AgentThinkingStepUpdate, {
        response_metadata: { tool_run_id: RUN, message: 'half done', tool_meta: { toolkit_name: 'jira' } },
      }),
      CONTEXT,
    );
    const action = actionsOf(history)[0];

    expect(String(action?.['message'])).toContain('half done');
    expect((action?.toolMeta ?? {})['toolkit_name']).toBe('jira');
  });

  it('ignores an update for a step it never saw', () => {
    const before = withAction();
    expect(
      applyChatStreamFrame(before, frame(SocketMessageType.AgentThinkingStepUpdate, { response_metadata: { tool_run_id: 'ghost' } }), CONTEXT),
    ).toBe(before);
  });

  describe('the agent_llm_end fan-out', () => {
    it('closes every step in one batch, not just the first', () => {
      // A pipeline with several LLM nodes reports them all in one frame.
      const history: readonly ChatMessage[] = [
        {
          ...pendingAssistant(),
          toolActions: [
            { id: 'a', type: 'tool', status: 'processing', parent_agent_name: 'root' } as ToolAction,
            { id: 'b', type: 'tool', status: 'processing', parent_agent_name: 'root' } as ToolAction,
          ],
        },
      ];
      const next = applyChatStreamFrame(
        history,
        frame(SocketMessageType.AgentLlmEnd, {
          response_metadata: {
            thinking_steps: [
              { tool_run_id: 'a', text: 'did a' },
              { tool_run_id: 'b', text: 'did b' },
            ],
          },
        }),
        CONTEXT,
      );

      expect(actionsOf(next).map((action) => action.status)).toEqual(['complete', 'complete']);
      expect(actionsOf(next)[0]?.['content']).toContain('did a');
    });

    it('drops an empty transition step rather than showing plumbing', () => {
      const history: readonly ChatMessage[] = [
        { ...pendingAssistant(), toolActions: [{ id: 'a', type: 'tool', status: 'processing' } as ToolAction] },
      ];
      const next = applyChatStreamFrame(
        history,
        frame(SocketMessageType.AgentLlmEnd, { response_metadata: { thinking_steps: [{ tool_run_id: 'a', text: '   ' }] } }),
        CONTEXT,
      );

      expect(actionsOf(next)).toHaveLength(0);
    });

    it('keeps an empty step that belongs to a parent agent', () => {
      const history: readonly ChatMessage[] = [
        {
          ...pendingAssistant(),
          toolActions: [{ id: 'a', type: 'tool', status: 'processing', parent_agent_name: 'planner' } as ToolAction],
        },
      ];
      const next = applyChatStreamFrame(
        history,
        frame(SocketMessageType.AgentLlmEnd, { response_metadata: { thinking_steps: [{ tool_run_id: 'a', text: '' }] } }),
        CONTEXT,
      );

      expect(actionsOf(next)).toHaveLength(1);
    });

    it('strips the verbose "with inputs {...}" tail the UI already renders', () => {
      const next = applyChatStreamFrame(
        withAction({ parent_agent_name: 'root' } as Partial<ToolAction>),
        frame(SocketMessageType.AgentLlmEnd, {
          response_metadata: { thinking_steps: [{ tool_run_id: RUN, text: 'called search with inputs {"q": 1}' }] },
        }),
        CONTEXT,
      );

      expect(String(actionsOf(next)[0]?.['content'])).not.toContain('with inputs');
      expect(String(actionsOf(next)[0]?.['content'])).toContain('called search');
    });

    it('recovers the run id from the message id when the step omits it', () => {
      const next = applyChatStreamFrame(
        withAction({ parent_agent_name: 'root' } as Partial<ToolAction>),
        frame(SocketMessageType.AgentLlmEnd, {
          response_metadata: { thinking_steps: [{ text: 'done', message: { id: `lc_run--${RUN}` } }] },
        }),
        CONTEXT,
      );

      expect(actionsOf(next)[0]?.status).toBe('complete');
    });

    it('closes the frame\'s own tool but leaves an approval-gated one alone', () => {
      const history: readonly ChatMessage[] = [
        {
          ...pendingAssistant(),
          toolActions: [
            { id: 'p', type: 'tool', status: 'processing' } as ToolAction,
            { id: 'g', type: 'tool', status: 'action_required' } as ToolAction,
          ],
        },
      ];
      let next = applyChatStreamFrame(history, frame(SocketMessageType.AgentLlmEnd, { response_metadata: { tool_run_id: 'p' } }), CONTEXT);
      next = applyChatStreamFrame(next, frame(SocketMessageType.AgentLlmEnd, { response_metadata: { tool_run_id: 'g' } }), CONTEXT);

      expect(actionsOf(next)[0]?.status).toBe('complete');
      expect(actionsOf(next)[1]?.status).toBe('action_required');
    });

    it('still stops the spinner when there are no steps at all', () => {
      const next = applyChatStreamFrame([pendingAssistant()], frame(SocketMessageType.AgentLlmEnd), CONTEXT);
      expect(next[0]?.isLoading).toBe(false);
    });
  });
});

describe('applyChatStreamFrame — interrupts', () => {
  function interruptsOf(history: readonly ChatMessage[]): readonly Record<string, unknown>[] {
    return (history[0]?.hitlInterrupts ?? []) as readonly Record<string, unknown>[];
  }

  describe('agent_hitl_interrupt', () => {
    it('stops the turn on a plain single pause and leaves the bubble text alone', () => {
      const history = applyChatStreamFrame(
        [{ ...pendingAssistant(), content: 'partial answer' }],
        frame(SocketMessageType.AgentHitlInterrupt, {
          response_metadata: {
            node_name: 'approve_delete',
            thread_id: 'thread-1',
            hitl_interrupt: { tool_name: 'delete_file', tool_call_id: 'call-1', action_label: 'Delete report.pdf' },
          },
        }),
        CONTEXT,
      );

      expect(history[0]?.isStreaming).toBe(false);
      expect(history[0]?.isLoading).toBe(false);
      // Overwriting content would leave a "requires approval" line in the
      // bubble after the user resumes and the real answer streams in.
      expect(history[0]?.content).toBe('partial answer');
      expect(history[0]?.threadId).toBe('thread-1');
    });

    it('leaves hitlInterrupts UNSET for a single pause so resume stays sequential', () => {
      // The consumer reads the array's mere presence as "parallel" and switches
      // to the hitl_decisions resume shape; populating it here would misroute a
      // pause the backend expects to answer with a single hitl_action.
      const history = applyChatStreamFrame(
        [pendingAssistant()],
        frame(SocketMessageType.AgentHitlInterrupt, {
          response_metadata: { hitl_interrupt: { tool_name: 'delete_file' } },
        }),
        CONTEXT,
      );

      expect(history[0]?.hitlInterrupts).toBeUndefined();
      expect(history[0]?.hitlInterrupt).toMatchObject({ tool_name: 'delete_file', resume_strategy: 'single' });
    });

    it('assembles a single pause from BOTH the top-level and nested fields', () => {
      // Reading only one of the two loses either the routing fields or the tool
      // detail the approval card renders.
      const history = applyChatStreamFrame(
        [pendingAssistant()],
        frame(SocketMessageType.AgentHitlInterrupt, {
          response_metadata: {
            node_name: 'guard',
            available_actions: ['approve'],
            edit_state_key: 'draft',
            interrupt_id: 'top-level-id',
            hitl_interrupt: { tool_name: 'send_email', guardrail_type: 'sensitive_tool', tool_call_id: 'call-9' },
          },
        }),
        CONTEXT,
      );

      expect(history[0]?.hitlInterrupt).toMatchObject({
        node_name: 'guard',
        available_actions: ['approve'],
        edit_state_key: 'draft',
        interrupt_id: 'top-level-id',
        tool_name: 'send_email',
        guardrail_type: 'sensitive_tool',
        tool_call_id: 'call-9',
      });
    });

    it('carries an ask_user pause’s questions off the nested interrupt', () => {
      // The native runtime's clarification pause (`guardrail_type:
      // clarifying_question`, `available_actions: ['answer']`) puts its
      // questions ONLY on the nested interrupt. The single-pause assembly used
      // to drop the field, so the card rendered the question text with no
      // option buttons, no input and no submit — the run could not be resumed.
      const questions = [
        { id: 'environment', question: 'Which environment?', options: [{ label: 'Staging' }], multiSelect: false },
      ];
      const history = applyChatStreamFrame(
        [pendingAssistant()],
        frame(SocketMessageType.AgentHitlInterrupt, {
          response_metadata: {
            message: 'Which environment?',
            available_actions: ['answer'],
            hitl_interrupt: {
              guardrail_type: 'clarifying_question',
              tool_name: 'ask_user',
              tool_call_id: 'call_mock_ask_user_1',
              interrupt_id: 'int-ask-1',
              questions,
            },
          },
        }),
        CONTEXT,
      );

      expect(history[0]?.hitlInterrupt).toMatchObject({
        guardrail_type: 'clarifying_question',
        available_actions: ['answer'],
        tool_call_id: 'call_mock_ask_user_1',
        interrupt_id: 'int-ask-1',
        questions,
      });
    });

    it('KEEPS streaming for a fan-out child and accumulates its siblings', () => {
      // Siblings are still running. Flipping isStreaming off collapses the live
      // thinking view and hides every sub-agent that has not rendered a card of
      // its own — including ones that finished without pausing.
      const childFrame = (name: string, thread: string, call: string) =>
        frame(SocketMessageType.AgentHitlInterrupt, {
          response_metadata: {
            metadata: { parent_agent_name: name, child_thread_id: thread },
            hitl_interrupt: { tool_call_id: call },
          },
        });

      let history = applyChatStreamFrame([{ ...pendingAssistant(), isStreaming: false }], childFrame('planner', 't-a', 'c-a'), CONTEXT);
      history = applyChatStreamFrame(history, childFrame('planner', 't-b', 'c-b'), CONTEXT);

      // Re-armed even though the parent's park-by-return had already stopped it.
      expect(history[0]?.isStreaming).toBe(true);
      expect(history[0]?.isRegenerating).toBe(false);
      expect(interruptsOf(history)).toHaveLength(2);
      expect(interruptsOf(history).map((entry) => entry['thread_id'])).toEqual(['t-a', 't-b']);
    });

    it('does not park the message on whichever child paused last', () => {
      // A child resumes on its OWN thread, carried per entry. Writing it to
      // msg.threadId would misroute the parent's resume.
      const history = applyChatStreamFrame(
        [{ ...pendingAssistant(), threadId: 'parent-thread' }],
        frame(SocketMessageType.AgentHitlInterrupt, {
          response_metadata: {
            metadata: { parent_agent_name: 'planner', child_thread_id: 'child-thread', thread_id: 'child-thread' },
            hitl_interrupt: { tool_call_id: 'c-1' },
          },
        }),
        CONTEXT,
      );

      expect(history[0]?.threadId).toBe('parent-thread');
      expect(interruptsOf(history)[0]?.['thread_id']).toBe('child-thread');
    });

    it('keeps streaming for an in-process parallel aggregate and populates the array', () => {
      // N paused sub-agents in ONE frame, each labelled with its parent but
      // with no child thread — so it is not the fan-out shape, yet the run is
      // just as much still active.
      const history = applyChatStreamFrame(
        [pendingAssistant()],
        frame(SocketMessageType.AgentHitlInterrupt, {
          response_metadata: {
            hitl_interrupts: [
              { parent_agent_name: 'researcher', tool_call_id: 'c-1' },
              { parent_agent_name: 'writer', tool_call_id: 'c-2' },
            ],
          },
        }),
        CONTEXT,
      );

      expect(history[0]?.isStreaming).toBe(true);
      expect(interruptsOf(history).map((entry) => entry['tool_call_id'])).toEqual(['c-1', 'c-2']);
      // The singular field tracks the first still-pending entry.
      expect(history[0]?.hitlInterrupt).toMatchObject({ tool_call_id: 'c-1' });
    });

    it('stops the turn for a backend aggregate whose entries name no parent', () => {
      // Same array shape, but nothing is running behind it — this must behave
      // like a plain pause, which is why the check is on parent_agent_name
      // rather than on the array's presence.
      const history = applyChatStreamFrame(
        [pendingAssistant()],
        frame(SocketMessageType.AgentHitlInterrupt, {
          response_metadata: { hitl_interrupts: [{ tool_call_id: 'c-1' }] },
        }),
        CONTEXT,
      );

      expect(history[0]?.isStreaming).toBe(false);
      expect(interruptsOf(history)).toHaveLength(1);
    });

    it('produces entries the resume path can route by tool_call_id', () => {
      // The contract deriveHitlChildThreadId reads: match on tool_call_id, then
      // take thread_id / child_thread_id.
      const history = applyChatStreamFrame(
        [pendingAssistant()],
        frame(SocketMessageType.AgentHitlInterrupt, {
          response_metadata: {
            metadata: { parent_agent_name: 'planner', child_thread_id: 'child-7' },
            hitl_interrupt: { tool_call_id: 'call-7' },
          },
        }),
        CONTEXT,
      );

      const entry = interruptsOf(history).find((item) => item['tool_call_id'] === 'call-7');
      expect(entry?.['thread_id']).toBe('child-7');
      expect(entry?.['child_thread_id']).toBe('child-7');
    });
  });

  describe('agent_requires_confirmation', () => {
    it('keeps the bubble streaming in mono chat and releases it otherwise', () => {
      const confirmation = frame(SocketMessageType.AgentRequiresConfirmation, { content: 'Show more' });

      const mono = applyChatStreamFrame([pendingAssistant()], confirmation, { ...CONTEXT, isMonoChatting: true });
      const multi = applyChatStreamFrame([pendingAssistant()], confirmation, { ...CONTEXT, isMonoChatting: false });

      expect(mono[0]?.isStreaming).toBe(true);
      expect(multi[0]?.isStreaming).toBe(false);
      expect(mono[0]?.isLoading).toBe(false);
    });

    it('labels the button from the frame and falls back to Continue', () => {
      const withLabel = applyChatStreamFrame(
        [pendingAssistant()],
        frame(SocketMessageType.AgentRequiresConfirmation, { content: 'Show more' }),
        CONTEXT,
      );
      const without = applyChatStreamFrame(
        [pendingAssistant()],
        frame(SocketMessageType.AgentRequiresConfirmation),
        CONTEXT,
      );

      expect(withLabel[0]?.requiresConfirmation?.buttonText).toBe('Show more');
      expect(without[0]?.requiresConfirmation?.buttonText).toBe('Continue');
      expect(without[0]?.requiresConfirmation?.message).toContain('Token limit reached');
    });

    it('keeps the thread the earlier frames of this message established', () => {
      // Blanking it would strand the continue request with nowhere to resume.
      const history = applyChatStreamFrame(
        [{ ...pendingAssistant(), threadId: 'thread-earlier' }],
        frame(SocketMessageType.AgentRequiresConfirmation),
        CONTEXT,
      );

      expect(history[0]?.threadId).toBe('thread-earlier');
    });
  });

  describe('mcp_authorization_required', () => {
    const authFrame = (metadata: Record<string, unknown>) =>
      frame(SocketMessageType.McpAuthorizationRequired, {
        content: 'Authorization required.',
        response_metadata: { tool_run_id: 'mcp-1', tool_name: 'Jira MCP', server_url: 'https://mcp.example', ...metadata },
      });

    function outputs(history: readonly ChatMessage[]): Record<string, unknown> {
      return (((history[0]?.toolActions ?? [])[0] as ToolAction | undefined)?.['toolOutputs'] ?? {}) as Record<
        string,
        unknown
      >;
    }

    it('asks for action when the server advertised an authorization server', () => {
      const history = applyChatStreamFrame(
        [pendingAssistant()],
        authFrame({ authorization_servers: ['https://auth.example'] }),
        CONTEXT,
      );
      const action = (history[0]?.toolActions ?? [])[0] as ToolAction;

      expect(action.status).toBe('action_required');
      expect(action['content']).toContain('Authorization servers: https://auth.example');
      // Nothing is in flight: the user has to act.
      expect(history[0]?.isStreaming).toBe(false);
      expect(history[0]?.isLoading).toBe(false);
    });

    it('errors instead when the server advertised none', () => {
      // There is no authorization to start, so an action_required card would be
      // a button that cannot do anything.
      const history = applyChatStreamFrame([pendingAssistant()], authFrame({ status: 403 }), CONTEXT);
      const action = (history[0]?.toolActions ?? [])[0] as ToolAction;

      expect(action.status).toBe('error');
      expect(action['content']).toContain('403: Authorization error in "Jira MCP" toolkit.');
    });

    it('stores the token under the key the backend will look it up by', () => {
      // A wrong key stores a token the toolkit never finds, so the user is
      // asked to authorize again on every single call.
      const prebuilt = applyChatStreamFrame(
        [pendingAssistant()],
        authFrame({ authorization_servers: ['https://auth.example'], toolkit_type: 'mcp_github' }),
        CONTEXT,
      );
      expect(outputs(prebuilt)['server_url']).toBe('mcp_github');

      const composite = applyChatStreamFrame(
        [pendingAssistant()],
        authFrame({
          resource_metadata: { authorization_servers: ['https://auth.example'], configuration_uuid: 'cfg-1' },
        }),
        CONTEXT,
      );
      expect(outputs(composite)['server_url']).toBe('cfg-1:https://auth.example');

      const sharepoint = applyChatStreamFrame(
        [pendingAssistant()],
        authFrame({ resource_metadata: { authorization_servers: ['https://auth.example'], resource_name: 'SharePoint' } }),
        CONTEXT,
      );
      expect(outputs(sharepoint)['server_url']).toBe('https://auth.example');

      const plain = applyChatStreamFrame(
        [pendingAssistant()],
        authFrame({ authorization_servers: ['https://auth.example'], toolkit_type: 'mcp' }),
        CONTEXT,
      );
      expect(outputs(plain)['server_url']).toBe('https://mcp.example');
    });

    it('updates the existing card on a repeat rather than stacking a second one', () => {
      let history = applyChatStreamFrame([pendingAssistant()], authFrame({ status: 403 }), CONTEXT);
      history = applyChatStreamFrame(history, authFrame({ authorization_servers: ['https://auth.example'] }), CONTEXT);

      expect(history[0]?.toolActions).toHaveLength(1);
      expect(((history[0]?.toolActions ?? [])[0] as ToolAction).status).toBe('action_required');
    });

    it('renders every exact request from a parallel terminal event without duplicates', () => {
      const request = (interruptId: string, toolCallId: string) => ({
        interrupt_id: interruptId,
        tool_call_id: toolCallId,
        guardrail_type: 'mcp_auth',
        tool_name: 'SharePoint search',
        toolkit_type: 'sharepoint',
        server_url: 'https://sharepoint.example',
        resource_metadata: {
          resource_name: 'SharePoint',
          authorization_servers: ['https://login.example'],
        },
      });
      const terminal = authFrame({
        authorization_requests: [request('auth-1', 'call-1'), request('auth-2', 'call-2')],
      });
      let history = applyChatStreamFrame([pendingAssistant()], terminal, CONTEXT);
      history = applyChatStreamFrame(history, terminal, CONTEXT);

      expect(history[0]?.toolActions).toHaveLength(2);
      expect(history[0]?.toolActions?.map(
        (action) => (action as ToolAction & { authorizationRequestId?: string }).authorizationRequestId,
      )).toEqual(['auth-1', 'auth-2']);
    });
  });
});

describe('applyChatStreamFrame — swarm', () => {
  const CHILD_ID = 'child-message-1';

  function swarmFrame(extra: Record<string, unknown> = {}) {
    // NOTE the shape: message_id is the CHILD's own id, and parent_message_id
    // names the assistant message the answer belongs to.
    return {
      type: SocketMessageType.SwarmChildMessage,
      message_id: CHILD_ID,
      parent_message_id: MESSAGE_ID,
      agent_name: 'researcher',
      created_at: '2026-08-13 12:00:00',
      ...extra,
    };
  }

  function swarmActions(history: readonly ChatMessage[]): readonly ToolAction[] {
    return (history[0]?.toolActions ?? []) as readonly ToolAction[];
  }

  it('attaches the child answer to the PARENT, not to the id the frame leads with', () => {
    // findTarget resolves by message_id, which here is the CHILD's — using it
    // would miss every time and drop the sub-agent's whole answer.
    const history = applyChatStreamFrame([pendingAssistant()], swarmFrame({ content: 'I found three sources.' }), CONTEXT);

    expect(history).toHaveLength(1);
    expect(history[0]?.id).toBe(MESSAGE_ID);
    expect(swarmActions(history)).toHaveLength(1);
    expect(swarmActions(history)[0]).toMatchObject({
      id: CHILD_ID,
      name: 'researcher',
      type: 'swarm_child',
      status: 'complete',
      content: 'I found three sources.',
      toolOutputs: 'I found three sources.',
      toolMeta: { agent_name: 'researcher' },
    });
  });

  it('falls back to the in-flight message when the parent id has not propagated', () => {
    // A child can report before its parent's id is known. Losing a sub-agent's
    // whole answer is worse than attaching it to the turn in progress.
    const history = applyChatStreamFrame(
      [pendingAssistant()],
      swarmFrame({ parent_message_id: undefined, content: 'partial finding' }),
      CONTEXT,
    );

    expect(swarmActions(history)).toHaveLength(1);
  });

  it('drops a tool_use-only message instead of adding an empty accordion', () => {
    // Anthropic content blocks: a child that only called tools has no prose,
    // and an entry per tool call would bury the answers that do exist.
    const before: readonly ChatMessage[] = [pendingAssistant()];
    const toolUseOnly = applyChatStreamFrame(
      before,
      swarmFrame({ content: [{ type: 'tool_use', name: 'search', input: {} }] }),
      CONTEXT,
    );
    const whitespace = applyChatStreamFrame(before, swarmFrame({ content: '   ' }), CONTEXT);

    expect(toolUseOnly).toBe(before);
    expect(whitespace).toBe(before);
  });

  it('keeps the text blocks out of a mixed content array and joins them', () => {
    const history = applyChatStreamFrame(
      [pendingAssistant()],
      swarmFrame({
        content: [{ text: 'first' }, { type: 'tool_use', name: 'search' }, { text: 'second' }],
      }),
      CONTEXT,
    );

    // The tool_use block contributes nothing — stringifying it would put raw
    // JSON in the sub-agent accordion.
    expect(swarmActions(history)[0]?.['content']).toBe('first\nsecond');
  });

  it('reads text from a bare object and stringifies one that has none', () => {
    const withText = applyChatStreamFrame([pendingAssistant()], swarmFrame({ content: { text: 'from object' } }), CONTEXT);
    expect(swarmActions(withText)[0]?.['content']).toBe('from object');

    const withoutText = applyChatStreamFrame([pendingAssistant()], swarmFrame({ content: { finding: 'x' } }), CONTEXT);
    expect(String(swarmActions(withoutText)[0]?.['content'])).toContain('finding');
  });

  it('normalises the Postgres-style timestamp rather than producing an Invalid Date', () => {
    const history = applyChatStreamFrame([pendingAssistant()], swarmFrame({ content: 'done' }), CONTEXT);
    const action = swarmActions(history)[0];

    expect(action?.['created_at']).toBe(Date.parse('2026-08-13T12:00:00Z'));
    expect(action?.['ended_at']).toBe(action?.['created_at']);
    expect(action?.['timestamp']).toBe(action?.['created_at']);
  });

  it('falls back to the injected clock when the frame carries no timestamp', () => {
    const history = applyChatStreamFrame(
      [pendingAssistant()],
      swarmFrame({ created_at: undefined, content: 'done' }),
      CONTEXT,
    );

    expect(swarmActions(history)[0]?.['created_at']).toBe(Date.parse('2026-08-13T00:00:00.000Z'));
  });

  it('accumulates one entry per child rather than replacing the previous one', () => {
    let history = applyChatStreamFrame([pendingAssistant()], swarmFrame({ content: 'from researcher' }), CONTEXT);
    history = applyChatStreamFrame(
      history,
      swarmFrame({ message_id: 'child-message-2', agent_name: 'writer', content: 'from writer' }),
      CONTEXT,
    );

    expect(swarmActions(history).map((action) => action.name)).toEqual(['researcher', 'writer']);
  });

  it('leaves state untouched when no message can own the child', () => {
    const before: readonly ChatMessage[] = [{ ...pendingAssistant(), isStreaming: false, isLoading: false, id: 'other' }];
    expect(applyChatStreamFrame(before, swarmFrame({ content: 'orphan' }), CONTEXT)).toBe(before);
  });

  it('leaves the raw swarm lifecycle frames inert', () => {
    // The baseline states outright that swarm work renders from
    // swarm_child_message alone; reducing these too would be a second, partial
    // source for the same accordions.
    const before: readonly ChatMessage[] = [pendingAssistant()];
    for (const type of [
      SocketMessageType.AgentSwarmAgentStart,
      SocketMessageType.AgentSwarmAgentResponse,
      SocketMessageType.AgentSwarmHandoff,
    ]) {
      expect(applyChatStreamFrame(before, frame(type, { content: 'x' }), CONTEXT), type).toBe(before);
    }
  });
});

describe('applyChatStreamFrame — summaries', () => {
  const startFrame = (extra: Record<string, unknown> = {}) =>
    frame(SocketMessageType.ChatPredictSummaryStarted, {
      // The run id and model come in on `payload`, not response_metadata.
      payload: { response_metadata: { tool_run_id: 'run-sum' }, llm_settings: { model_name: 'gpt-4o' } },
      ...extra,
    });

  function actions(history: readonly ChatMessage[]): readonly ToolAction[] {
    return (history[0]?.toolActions ?? []) as readonly ToolAction[];
  }

  it('adds a processing summary entry built from payload, not response_metadata', () => {
    const history = applyChatStreamFrame([pendingAssistant()], startFrame(), CONTEXT);

    expect(actions(history)).toHaveLength(1);
    expect(actions(history)[0]).toMatchObject({
      id: 'thinking_step_run-sum',
      type: 'summary',
      status: 'processing',
      message: 'Summarizing the chat history...',
      toolMeta: { ls_model_name: 'gpt-4o' },
    });
  });

  it('gives an id-less summary a unique id instead of one shared literal', () => {
    // The baseline's `'thinking_step_' + id || '' + v4()` is always truthy, so
    // its uuid fallback never ran and every id-less summary collided.
    const first = applyChatStreamFrame([pendingAssistant()], startFrame({ payload: {} }), CONTEXT);
    const second = applyChatStreamFrame([pendingAssistant()], startFrame({ payload: {} }), CONTEXT);

    expect(actions(first)[0]?.id).not.toBe(actions(second)[0]?.id);
    expect(actions(first)[0]?.id).not.toBe('thinking_step_undefined');
  });

  it('clears the previous answer AND every pause left over from it', () => {
    // Summarising resumes a turn. A stale approval card surviving the reset
    // would still be clickable against a run that has moved on.
    const history = applyChatStreamFrame(
      [
        {
          ...pendingAssistant(),
          content: 'previous answer',
          references: ['r'],
          isStreaming: false,
          isLoading: false,
          hitlInterrupt: { tool_name: 'x' },
          hitlInterrupts: [{ tool_name: 'x' }],
          requiresConfirmation: { message: 'm', buttonText: 'Continue' },
        },
      ],
      startFrame(),
      CONTEXT,
    );

    expect(history[0]?.content).toBe('');
    expect(history[0]?.references).toEqual([]);
    expect(history[0]?.hitlInterrupt).toBeUndefined();
    expect(history[0]?.hitlInterrupts).toBeUndefined();
    expect(history[0]?.requiresConfirmation).toBeUndefined();
    // Back in flight: the answer is still coming after the summary.
    expect(history[0]?.isStreaming).toBe(true);
    expect(history[0]?.isLoading).toBe(true);
  });

  it('keeps existing tool actions rather than replacing the timeline', () => {
    const history = applyChatStreamFrame(
      [{ ...pendingAssistant(), toolActions: [{ id: 'earlier', type: 'tool', status: 'complete' } as ToolAction] }],
      startFrame(),
      CONTEXT,
    );

    expect(actions(history).map((action) => action.id)).toEqual(['earlier', 'thinking_step_run-sum']);
  });

  it('links the reply to the question it answers, and does not overwrite an existing link', () => {
    const fresh = applyChatStreamFrame([pendingAssistant()], startFrame(), CONTEXT);
    expect(fresh[0]?.replyToId).toBe(QUESTION_ID);

    const already = applyChatStreamFrame([{ ...pendingAssistant(), replyToId: 'earlier-question' }], startFrame(), CONTEXT);
    expect(already[0]?.replyToId).toBe('earlier-question');
  });

  it('creates the message when the summary starts before it exists', () => {
    const history = applyChatStreamFrame([], startFrame(), CONTEXT);

    expect(history).toHaveLength(1);
    expect(history[0]?.role).toBe('assistant');
    expect(actions(history)).toHaveLength(1);
  });

  it('closes the summary by type and status, since the finish frame carries no run id', () => {
    // An id lookup would find nothing and leave the summary spinning for the
    // rest of the conversation.
    let history = applyChatStreamFrame([pendingAssistant()], startFrame(), CONTEXT);
    history = applyChatStreamFrame(history, frame(SocketMessageType.ChatPredictSummaryFinished), CONTEXT);

    expect(actions(history)[0]).toMatchObject({ status: 'complete', content: '', message: undefined });
    expect(actions(history)[0]?.['ended_at']).toBe(Date.parse('2026-08-13T00:00:00.000Z'));
  });

  it('leaves an already-finished summary and other tools alone', () => {
    const before: readonly ChatMessage[] = [
      {
        ...pendingAssistant(),
        toolActions: [
          { id: 's1', type: 'summary', status: 'complete' } as ToolAction,
          { id: 't1', type: 'tool', status: 'processing' } as ToolAction,
        ],
      },
    ];
    // No processing summary to close: the frame must not settle the running
    // tool that happens to sit next to it.
    expect(applyChatStreamFrame(before, frame(SocketMessageType.ChatPredictSummaryFinished), CONTEXT)).toBe(before);
  });

  it('closes only the summary when a tool is running alongside it', () => {
    let history = applyChatStreamFrame(
      [{ ...pendingAssistant(), toolActions: [{ id: 't1', type: 'tool', status: 'processing' } as ToolAction] }],
      startFrame(),
      CONTEXT,
    );
    history = applyChatStreamFrame(history, frame(SocketMessageType.ChatPredictSummaryFinished), CONTEXT);

    expect(actions(history).map((action) => action.status)).toEqual(['processing', 'complete']);
  });
});

describe('applyChatStreamFrame — chat_user_message', () => {
  const USER_UUID = 'question-uuid-1';
  // The real participant shape: `id` is the chat_participants ROW id and
  // `entity_meta.id` is the AUTH USER id — two different values, which is the
  // whole point (measured on the live stack: row 1 / user 6).
  const PARTICIPANTS = [
    { id: 'p-author', entity_meta: { id: 6, email: 'alice@example.com' }, meta: { user_name: 'Alice', user_avatar: 'alice.png' } },
    { id: 'p-agent', meta: { user_name: 'Support Agent' } },
  ];
  const ECHO_CONTEXT: ChatStreamContext = { ...CONTEXT, participants: PARTICIPANTS };

  const echo = (extra: Record<string, unknown> = {}) => ({
    type: SocketMessageType.ChatUserMessage,
    uuid: USER_UUID,
    author_participant_id: 'p-author',
    sent_to_id: 'p-agent',
    content: 'what is the status?',
    created_at: '2026-08-13 09:30:00',
    ...extra,
  });

  it('appends a user message attributed to its author', () => {
    const history = applyChatStreamFrame([], echo(), ECHO_CONTEXT);

    expect(history).toHaveLength(1);
    expect(history[0]).toMatchObject({
      id: USER_UUID,
      role: 'user',
      name: 'Alice',
      avatar: 'alice.png',
      content: 'what is the status?',
      // The AUTH USER id off the roster, NOT the `author_participant_id` the
      // frame states. Every other producer of `ChatMessage.userId`
      // (`normalise.ts`'s `userOptionalFields`, `buildOptimisticUserMessage`)
      // writes the auth id, and every reader (`ChatMessageList`'s edit/delete
      // gating, `entities/message`'s `canDeleteMessage`) compares against it —
      // stamping the participant id here made a live question unmatchable by
      // its own author, which reads on screen as its edit and delete controls
      // silently disappearing.
      userId: '6',
      participantId: 'p-agent',
      sentTo: PARTICIPANTS[1],
    });
    // Stated explicitly: the two ids are genuinely different values, so this
    // case cannot pass by accident on a fixture where they coincide.
    expect(history[0]?.userId).not.toBe('p-author');
  });

  it('states NO author id when the roster cannot resolve the author', () => {
    // Never the participant id as a fallback: it would restate the very
    // mismatch above, and an unattributed row is the honest answer.
    const unknownAuthor = applyChatStreamFrame([], echo({ author_participant_id: 'ghost' }), ECHO_CONTEXT);
    expect(unknownAuthor[0]?.userId).toBeUndefined();

    // A resolved author who carries no entity_meta (the agent participant) is
    // the same case.
    const agentAuthor = applyChatStreamFrame([], echo({ author_participant_id: 'p-agent' }), ECHO_CONTEXT);
    expect(agentAuthor[0]?.userId).toBeUndefined();
  });

  // ── the two id spellings that really meet here ─────────────────────────
  //
  // `ChatStreamFrame` types `author_participant_id`/`sent_to_id` as
  // `string | number` because both spellings are on the wire: the Go payloads
  // state participant ids as NUMBERS, the socket-era payloads this vocabulary
  // was captured from stated them as STRINGS. The persisted path
  // (`normalise.ts`'s `normaliseUserMessage`) compares `String()` on BOTH
  // sides for exactly this reason; the live path used a strict `===`, which
  // resolves nobody the moment the two spellings cross.
  //
  // Each case asserts name, avatar AND userId together, because ONE comparison
  // governs all three: an unresolved author is not a missing display name, it
  // is a question that also loses the auth id its edit and delete controls
  // gate on. `sentTo` rides the same lookup, so it is asserted too.
  //
  // `expect.soft` on purpose: a hard assertion stops at `name` and reports the
  // loss as cosmetic, which is the misreading this whole case exists to
  // prevent — the failure output has to show all four fields going at once.
  const NUMERIC_ID_AUTHOR = { user_name: 'Alice', user_avatar: 'alice.png' };

  it('resolves a NUMERIC frame id against STRING participant ids', () => {
    const stringRoster = [
      { id: '1', entity_meta: { id: 6 }, meta: NUMERIC_ID_AUTHOR },
      { id: '2', meta: { user_name: 'Support Agent' } },
    ];
    const history = applyChatStreamFrame(
      [],
      echo({ author_participant_id: 1, sent_to_id: 2 }),
      { ...CONTEXT, participants: stringRoster },
    );

    expect.soft(history[0]?.name).toBe('Alice');
    expect.soft(history[0]?.avatar).toBe('alice.png');
    expect.soft(history[0]?.userId).toBe('6');
    expect.soft(history[0]?.sentTo).toBe(stringRoster[1]);
  });

  it('resolves a STRING frame id against NUMERIC participant ids', () => {
    // The roster spelling the Go payloads actually produce, which
    // `MessageParticipantWire.id: string` does not describe — hence the cast.
    const numericRoster = [
      { id: 1, entity_meta: { id: 6 }, meta: NUMERIC_ID_AUTHOR },
      { id: 2, meta: { user_name: 'Support Agent' } },
    ] as unknown as NonNullable<ChatStreamContext['participants']>;
    const history = applyChatStreamFrame(
      [],
      echo({ author_participant_id: '1', sent_to_id: '2' }),
      { ...CONTEXT, participants: numericRoster },
    );

    expect.soft(history[0]?.name).toBe('Alice');
    expect.soft(history[0]?.avatar).toBe('alice.png');
    expect.soft(history[0]?.userId).toBe('6');
    expect.soft(history[0]?.sentTo).toBe(numericRoster[1]);
  });

  it('keys off uuid, not message_id', () => {
    // message_id on this frame does not name the user message; resolving by it
    // would attach the question to the assistant turn instead.
    const history = applyChatStreamFrame([pendingAssistant()], echo({ message_id: MESSAGE_ID }), ECHO_CONTEXT);

    expect(history).toHaveLength(2);
    expect(history[1]?.id).toBe(USER_UUID);
    expect(history[0]?.role).toBe('assistant');
  });

  it('does NOT render the same question twice when the echo repeats', () => {
    // The baseline appends unconditionally, while its own
    // addMessageToChatHistory guards the assistant path against exactly this.
    // Reproducing the omission would show the user their own message twice.
    let history = applyChatStreamFrame([], echo(), ECHO_CONTEXT);
    history = applyChatStreamFrame(history, echo({ content: 'edited question' }), ECHO_CONTEXT);

    expect(history).toHaveLength(1);
    expect(history[0]?.content).toBe('edited question');
  });

  it('normalises the timestamp to the ISO string the persisted path produces', () => {
    // Not the baseline's epoch-ms number: a live question and a replayed one
    // must not render their time in two different formats.
    const history = applyChatStreamFrame([], echo(), ECHO_CONTEXT);
    expect(history[0]?.createdAt).toBe('2026-08-13T09:30:00Z');
  });

  it('falls back to the injected clock when the frame has no timestamp', () => {
    const history = applyChatStreamFrame([], echo({ created_at: undefined }), ECHO_CONTEXT);
    expect(history[0]?.createdAt).toBe('2026-08-13T00:00:00.000Z');
  });

  it('survives an unknown author and an empty roster', () => {
    const unknownAuthor = applyChatStreamFrame([], echo({ author_participant_id: 'ghost' }), ECHO_CONTEXT);
    expect(unknownAuthor[0]?.name).toBe('');
    expect(unknownAuthor[0]?.sentTo).toBe(PARTICIPANTS[1]);

    const noRoster = applyChatStreamFrame([], echo(), CONTEXT);
    expect(noRoster).toHaveLength(1);
    expect(noRoster[0]?.name).toBe('');
    expect(noRoster[0]?.sentTo).toBeUndefined();
  });

  it('carries the message items through', () => {
    const items = [{ id: 2 }, { id: 1 }] as unknown as ChatMessage['messageItems'];
    const history = applyChatStreamFrame([], echo({ message_items: items }), ECHO_CONTEXT);
    expect(history[0]?.messageItems).toBe(items);
  });

  it('ignores an echo that identifies no message', () => {
    const before: readonly ChatMessage[] = [pendingAssistant()];
    expect(applyChatStreamFrame(before, echo({ uuid: undefined }), ECHO_CONTEXT)).toBe(before);
  });
});

describe('a whole-message frame after the streamed chunks', () => {
  // The exact sequence a browser receives, captured from a live standalone
  // stack by wrapping window.EventSource. The reply is assembled by the chunk
  // frames and then sent AGAIN, in full, by agent_response — appending both
  // rendered every answer twice (#294).
  const RECORDED = [
    { type: SocketMessageType.AgentStart },
    { type: SocketMessageType.AgentOnTransitionalEdge },
    { type: SocketMessageType.AgentLlmStart },
    { type: SocketMessageType.AgentLlmChunk, content: 'MOCK: ' },
    { type: SocketMessageType.AgentLlmChunk, content: 'dbg ' },
    { type: SocketMessageType.AgentLlmChunk, content: '1786658174033 ' },
    { type: SocketMessageType.AgentLlmEnd },
    { type: 'partial_message' },
    { type: SocketMessageType.AgentOnTransitionalEdge },
    { type: SocketMessageType.PipelineFinish, content: 'MOCK: dbg 1786658174033 ' },
    { type: SocketMessageType.AgentResponse, content: 'MOCK: dbg 1786658174033 ' },
    { type: 'full_message', content: 'MOCK: dbg 1786658174033 ' },
  ];

  it('renders the reply ONCE', () => {
    const result = RECORDED.reduce<readonly ChatMessage[]>(
      (history, next) => applyChatStreamFrame(history, frame(next.type, next.content !== undefined ? { content: next.content } : {}), CONTEXT),
      [pendingAssistant()],
    );

    expect(result).toHaveLength(1);
    expect(result[0]?.content).toBe('MOCK: dbg 1786658174033 ');
    expect(result[0]?.isStreaming).toBe(false);
  });

  it('still appends a DISTINCT intermediate response', () => {
    // The guard is on what is already rendered, not on the frame type, so a
    // pipeline that emits several different responses keeps all of them.
    let history = applyChatStreamFrame([pendingAssistant()], frame(SocketMessageType.AgentResponse, { content: 'step one' }), CONTEXT);
    history = applyChatStreamFrame(history, frame(SocketMessageType.AgentResponse, { content: 'step two' }), CONTEXT);

    expect(history[0]?.content).toContain('step one');
    expect(history[0]?.content).toContain('step two');
  });

  it('does not swallow a reply that merely repeats the last words', () => {
    // `endsWith` is exact: a response whose text is a SUFFIX of what is on
    // screen is suppressed, but one that only shares a prefix still appends.
    const history = applyChatStreamFrame(
      [{ ...pendingAssistant(), content: 'MOCK: hello' }],
      frame(SocketMessageType.AgentResponse, { content: 'MOCK: hello world' }),
      CONTEXT,
    );

    expect(history[0]?.content).toBe('MOCK: helloMOCK: hello world');
  });
});

describe('a streamed token is renderable the moment it arrives', () => {
  it('clears isLoading on the first delta, and keeps isStreaming', () => {
    // `ApplicationAnswer` gates the answer body on
    // `canRenderContent = !exception && !isLoadingOrRegenerating`. A chunk that
    // leaves `isLoading` true therefore accumulates text the UI refuses to
    // paint, and the whole reply appears in one step when the turn settles —
    // which is indistinguishable, on screen, from no streaming at all (#294).
    const history = applyChatStreamFrame(
      [pendingAssistant()],
      frame(SocketMessageType.AgentLlmChunk, { content: 'MOCK: ' }),
      CONTEXT,
    );

    expect(history[0]?.content).toBe('MOCK: ');
    expect(history[0]?.isLoading, 'a delta IS output; holding isLoading hides it').toBe(false);
    // The turn is still running: the stop control and spinner read isStreaming.
    expect(history[0]?.isStreaming).toBe(true);
  });

  it('still shows the spinner before any token arrives', () => {
    // agent_llm_start means "asked the model, nothing back yet" — that IS
    // loading, and flipping it early would drop the only affordance the user
    // has while waiting.
    const history = applyChatStreamFrame([pendingAssistant()], frame(SocketMessageType.AgentLlmStart), CONTEXT);

    expect(history[0]?.isLoading).toBe(true);
    expect(history[0]?.isStreaming).toBe(true);
  });
});
