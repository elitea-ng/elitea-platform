/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 */
export type {
  AssistantMessage,
  Message,
  RawSocketMessage,
  SocketMessageType,
  ToolAction,
  UserMessage,
} from './model/types';
export { canDeleteMessage, isMessageStreaming, isUserMessageRow } from './model/selectors';
export { convertTime, normaliseAssistantMessage, normaliseUserMessage } from './lib/normalise';
export { buildAuthorizationActions } from './lib/authorizationActions';
export type { MessageAuthorWire, MessageGroupWire, MessageParticipantWire } from './lib/wire';
