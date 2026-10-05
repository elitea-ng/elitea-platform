/**
 * hooks/useChatBoxData.seed.ts — who wins when the server seed and the live
 * transcript disagree.
 *
 * `useChatBoxData` keeps ONE piece of chat state (`conversationForSync`) that
 * has two writers: the streaming reducer, which folds `execution.node_event`
 * frames into it token by token, and a re-seed effect, which rebuilds it from
 * the conversation-messages query whenever that query's data — or the
 * conversation identity — changes. The rule below decides the collision. It is
 * a pure function in its own module so the rule can be asserted directly: the
 * effect that used to hold it inline could only be exercised through the whole
 * chat surface, which is why neither of the two defects it now closes was
 * caught by a unit test.
 *
 * DEFECT 1 (closed earlier, kept here): an EMPTY seed must not overwrite a live
 * transcript. The first send COMMITS the conversation before it tries any
 * transport and the page then routes to `/chat/{id}`, so adopting the row
 * re-ran the effect with a server answer that had no messages in it yet, and
 * the reader's own question — plus, on a refused turn, its failure bubble —
 * was erased a moment after being drawn.
 *
 * DEFECT 2 (closed now): a NON-EMPTY seed must not overwrite a transcript whose
 * LAST message is a turn still in flight. Measured on CI, not reasoned about:
 * in `chat-stream-rust` shard 3 the answer bubble painted `MOCK:` (the mock's
 * first token), lost it one screencast frame later, and finished the turn
 * holding every token EXCEPT the first — `autotest tts arm 262128` against a
 * stored answer of `MOCK: autotest tts arm 262128`. The durable replay
 * (`elitea_runtime.execution_replay_events`) held all eleven frames in order
 * and the persisted row held the whole answer, so nothing was lost on the wire
 * or in the database: the re-seed landed between the first and second
 * `agent_llm_chunk`, replaced the live message with the persisted row as it
 * stood at the time the messages query happened to have been fetched (content
 * `''`, still streaming), and the remaining chunks appended onto that empty
 * string. The window is a few tens of milliseconds wide, which is why it shows
 * up on a loaded CI runner and not on a developer laptop, and why it presented
 * as three flaky voice-TTS cases rather than as "streaming is broken".
 *
 * A turn in flight is therefore authoritative over the seed FOR ITS OWN
 * CONVERSATION. Switching conversation still re-seeds unconditionally — the
 * case the effect exists for — and once the turn settles (`isStreaming` and
 * `isLoading` both false on the last message, which every terminal frame and
 * every failure path writes) the seed wins again, so a stuck flag cannot
 * freeze the surface on stale data for longer than the turn it belongs to.
 */
import type { ChatMessage } from '@/features/chat-messages';

/**
 * Is the transcript's LAST message a turn that has not settled?
 *
 * The last message only, deliberately. `isStreaming` is written per message by
 * the stream reducer and cleared by the terminal frame, by `settleInFlight` on
 * a failure, and by the transport's connection-lost path; a row further up that
 * somehow kept the flag is history, not an in-flight turn, and letting it veto
 * every future re-seed would trade a lost token for a permanently stale
 * transcript.
 */
export function hasUnsettledTurn(history: readonly ChatMessage[]): boolean {
  const last = history[history.length - 1];
  if (last === undefined) return false;
  return last.isStreaming === true || last.isLoading === true;
}

/**
 * May a settled refetch replace the live transcript's in-flight last turn?
 *
 * Only when no stream ever wrote to that turn: the live copy is still an
 * object a seed drew (#6654). That is the reload-mid-turn case: the
 * first seed drew the turn as streaming, nothing settled it, and the old rule
 * kept that placeholder over every later refetch, even one holding the final
 * answer. A turn a stream owns is a NEW object (the reducer and the reattach
 * reset both copy it), and its stream may lag the database: replacing it with
 * the persisted text and then folding the late chunks onto that text would
 * print them twice. DEFECT 2's rule stands for that turn.
 */
export function seedSettlesLiveTurn(
  live: readonly ChatMessage[],
  seed: readonly ChatMessage[],
  wasSeeded: (message: ChatMessage) => boolean,
): boolean {
  const last = live[live.length - 1];
  if (last === undefined || !wasSeeded(last)) return false;
  const persisted = seed.find((message) => message.id === last.id);
  return persisted !== undefined && persisted.isStreaming !== true && persisted.isLoading !== true;
}

/**
 * The chat history to keep when the re-seed effect fires.
 *
 * @param live  the transcript on screen now (streamed tokens included)
 * @param seed  the transcript rebuilt from the conversation-messages query
 * @param switchedConversation  the reader really moved to a different conversation
 * @param wasSeeded  whether a message object came from a seed, for `seedSettlesLiveTurn`
 */
export function resolveSeededChatHistory(
  live: readonly ChatMessage[],
  seed: readonly ChatMessage[],
  switchedConversation: boolean,
  wasSeeded: (message: ChatMessage) => boolean = () => false,
): readonly ChatMessage[] {
  if (switchedConversation) return seed;
  if (live.length === 0) return seed;
  if (seed.length === 0) return live;
  if (hasUnsettledTurn(live) && !seedSettlesLiveTurn(live, seed, wasSeeded)) return live;
  return seed;
}
