import type { ChatMessage } from '@/features/chat-messages';

export type McpAuthorizationAction = 'authorize' | 'skip';

export interface McpAuthorizationDecision {
  readonly interruptId: string;
  readonly toolCallId?: string | undefined;
  readonly action: McpAuthorizationAction;
}

export interface McpAuthorizationBatch {
  readonly original: ChatMessage;
  readonly decisions: Map<string, McpAuthorizationDecision>;
}
