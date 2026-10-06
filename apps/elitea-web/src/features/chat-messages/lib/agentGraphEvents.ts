/**
 * lib/agentGraphEvents.ts — which stream frames reach the flow editor (issue #93).
 *
 * The graph slice of the baseline reducer
 * (`EliteaUI/src/components/Chat/hooks.js:1489-1496`) is six case labels — the
 * five graph types plus `pipeline_finish`, which the reducer handles separately
 * — sharing one body: `onRcvAgentEventRef.current({ ...message })`. The five
 * graph types change NO message state at all: `agent_on_*` frames drive the
 * pipeline flow editor's node highlighting and run timeline, which live outside
 * chat history entirely (`useRunEvent.ts` → `parseRunsByEvent.helpers.ts`, both
 * already ported).
 *
 * So this slice is a forwarding contract rather than reducer cases. The
 * reducer leaves every one of these frames inert — correctly, and not because
 * they are unported — while the caller that owns the socket forwards them on.
 * That consumer chain already exists in this app (`EditorPanel.onRcvAgentEvent`
 * → `FlowEditor`); what was missing is the rule for WHICH frames feed it, which
 * is what this module supplies.
 */
import { SocketMessageType } from './chatStreamFrame';

/**
 * The prefix the backend actually emits under.
 *
 * A prefix test, not a fixed list, because the worker emits more graph events
 * than the baseline names: `agent_events.py:41-47` maps SEVEN, including `on_loop_tool_node`
 * and `on_loop_node`, against the five the baseline lists by hand. Those two
 * reached the flow editor only through the baseline's `default` branch, and the
 * consumer still keys off the same prefix (`parseRunsByEvent.helpers.ts:121`),
 * so a closed list here would silently stop highlighting loop nodes.
 */
export function isAgentGraphEvent(type: string | undefined): boolean {
  return typeof type === 'string' && type.startsWith('agent_on');
}

/**
 * The frames the baseline forwards to `onRcvAgentEvent`, enumerated from its
 * switch rather than guessed at.
 *
 * The set is deliberately not "everything the reducer handles": `agent_llm_chunk`,
 * `agent_thinking_step_update`, `references`, `chat_user_message`, `error`,
 * `llm_error` and the swarm/summary frames are all handled by the reducer and
 * NOT forwarded. Forwarding them would feed the flow editor's run timeline
 * per-token chunks it has no entry for.
 */
const FORWARDED_TYPES: ReadonlySet<string> = new Set<string>([
  SocketMessageType.StartTask,
  SocketMessageType.AgentStart,
  SocketMessageType.AgentLlmStart,
  SocketMessageType.AgentLlmEnd,
  SocketMessageType.AgentResponse,
  SocketMessageType.AgentToolStart,
  SocketMessageType.AgentToolEnd,
  SocketMessageType.AgentToolError,
  SocketMessageType.AgentThinkingStep,
  SocketMessageType.AgentException,
  SocketMessageType.McpAuthorizationRequired,
  SocketMessageType.AgentRequiresConfirmation,
  SocketMessageType.AgentHitlInterrupt,
  SocketMessageType.AgentNodeRecoveryRequired,
  SocketMessageType.PipelineFinish,
]);

/**
 * Whether the caller should hand this frame to the flow editor.
 *
 * `agent_response` is in the set but its two fall-through siblings (`chunk`,
 * `ai_message_chunk`) are not: the baseline shares one case body for all three
 * and then re-tests `socketMessageType === AgentResponse` before forwarding
 * (`hooks.js:546-548`). Reading the shared body as "all three forward" is the
 * easy mistake, and it would push every streamed chunk at the run timeline.
 */
export function shouldForwardAgentEvent(type: string | undefined): boolean {
  if (typeof type !== 'string') return false;
  return FORWARDED_TYPES.has(type) || isAgentGraphEvent(type);
}
