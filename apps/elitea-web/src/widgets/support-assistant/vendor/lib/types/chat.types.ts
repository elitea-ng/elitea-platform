export type TMessage = {
  id: string;
  role: 'user' | 'assistant';
  content: string;
  timestamp: number;
  /*
   * `| undefined` on every optional member is required by this app's
   * `exactOptionalPropertyTypes`, under which `{ statusMessage: undefined }` is
   * NOT assignable to `{ statusMessage?: string }`. The reducer in
   * `chat.hook.ts` clears these fields by writing `undefined` into them
   * explicitly — the reference's own idiom — so the type has to admit it.
   */
  isStreaming?: boolean | undefined;
  isAnimating?: boolean | undefined;
  isError?: boolean | undefined;
  statusMessage?: string | undefined;
};

export type TConversationListItem = {
  id: number;
  uuid: string;
  name: string;
  is_private: boolean;
  author_id: number;
  created_at: string;
  updated_at: string;
  meta: {
    is_hidden: boolean;
    context_strategy: {
      name: string;
      enabled: boolean;
      created_at: string;
      last_optimized_at: string | null;
      max_context_tokens: number;
      enable_summarization: boolean;
      summary_instructions: string;
      summary_llm_settings: unknown;
      preserve_recent_messages: number;
      preserve_system_messages: boolean;
    };
    conversation_type: string;
  };
  source: string;
  attachment_participant_id: string | null;
  instructions: string | null;
  participants_count: number;
  message_groups_count: number;
  users_count: number;
  duration: number;
};

export type TConversationsResponse = {
  items: TConversationListItem[];
  total: number;
};

/* Referenced by TRawMessageGroup below; not part of the widget's public types. */
type TRawMessageItem = {
  item_type?: string;
  type?: string;
  item_details?: { content?: string };
  content?: string;
};

export type TRawMessageGroup = {
  uuid?: string;
  id?: string;
  /**
   * WHO THE GROUP WAS ADDRESSED TO, under BOTH names this widget can meet.
   *
   * The reference's socket.io payload carries `sent_to`. This service's REST
   * transcript carries `sent_to_id` — `ConversationsRepo.ListMessageGroups`
   * writes that key and never the short one. The vendored reader knew only the
   * short name, so every replayed message read as an assistant message: a
   * reopened conversation rendered the user's own questions as answers.
   */
  sent_to?: unknown;
  sent_to_id?: unknown;
  message_items?: TRawMessageItem[];
  created_at_ts?: number;
  created_at?: string;
};

export type TRawConversation = {
  uuid?: string;
  id?: string;
  name?: string;
  message_groups?: TRawMessageGroup[];
};

export type TSocketMessage = {
  message_id: string;
  type: string;
  content: unknown;
  response_metadata?: {
    finish_reason?: string;
    task_id?: string;
    participant_id?: string;
    [key: string]: unknown;
  };
  sio_event?: string;
};
