/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 *
 * This unit (C1) added 13 exports to the 7 already here (Wave 0/1's
 * conversation type/selectors/`createDraftConversation`), landing exactly
 * at the 20 cap. Per the C1 brief's own guidance ("export cohesive hook
 * bundles/objects where sensible rather than every individual mutation as
 * its own top-level export"), 6 of the 13 are curated OBJECT BUNDLES
 * (`conversationApi`, `contextManagementApi`, `conversationNavigation`,
 * `chatHelpers`, `newConversationHelpers` — each a plain object of related
 * functions, ONE named export per §3.5's `countExports` regardless of how
 * many properties it carries) rather than ~30 individual named exports for
 * the REST/helper layer this unit built (`../api/{conversationApi,
 * messageApi,contextManagementApi}.ts`, `./lib/{chat,newConversation}.
 * helpers.ts`, `./lib/hooks/navigation.ts`).
 *
 * **Type-export budget trade-off, disclosed:** only `ConversationWire` (the
 * central REST-layer wire shape) is re-exported by name here — the cap left
 * no room for the ~15 narrower param/result types (`NewConversationInput`,
 * `UploadedAttachment`, `ConversationNavigationTarget`, the 8
 * context-management param interfaces, etc.) each source file under `api/`
 * and `lib/hooks/` also exports. A future consumer that needs one of those
 * can import it directly from the concrete file (e.g. `@/entities/
 * conversation/lib/hooks/useUploadAttachments` for `UploadedAttachment`) —
 * a narrower, still-legal import path (everything under this slice is
 * still `entities/conversation/**`, just not re-curated through the
 * barrel) — until/unless a real cross-slice need justifies spending one of
 * the 20 slots on it, matching `entities/toolkit/index.ts`'s own
 * documented precedent for the identical trade-off.
 */
import {
  AGENT_CONTINUE_AUTHORIZATION_CONTRACT,
  AGENT_CONTINUE_HITL_CONTRACT,
  AGENT_CONTINUE_OUTPUT_LIMIT_CONTRACT,
  AGENT_EXECUTE_ADHOC_CONTRACT,
  AGENT_EXECUTE_APPLICATION_CONTRACT,
  AGENT_REGENERATE_CONTRACT,
  continueAgentExecution,
  conversationCreate,
  conversationDetails,
  conversationEdit,
  deleteConversation,
  regenerate,
  selectConversation,
  startAgentExecution,
  stopChatTask,
  unselectConversation,
  useConversationCreateMutation,
  useConversationDetailsQuery,
  useConversationEditMutation,
  useDeleteConversationMutation,
  useRegenerateMutation,
  useSelectConversationMutation,
  useStopChatTaskMutation,
  useUnselectConversationMutation,
} from './api/conversationApi';
import {
  deleteAllMessagesFromConversation,
  deleteMessageFromConversation,
  messageList,
  useDeleteAllMessagesFromConversationMutation,
  useDeleteMessageFromConversationMutation,
  useMessageListQuery,
} from './api/messageApi';
import {
  deleteSummary,
  generateSummary,
  getContextAnalytics,
  getContextStatus,
  getConversationSummaries,
  optimizeContext,
  updateContextStrategy,
  updateSummary,
  useDeleteSummaryMutation,
  useGenerateSummaryMutation,
  useGetContextAnalyticsQuery,
  useGetContextStatusQuery,
  useGetConversationSummariesQuery,
  useOptimizeContextMutation,
  useUpdateContextStrategyMutation,
  useUpdateSummaryMutation,
} from './api/contextManagementApi';
import {
  buildClearConversationUrl,
  buildConversationUrlChange,
  buildCreateConversationUrl,
  buildResetSearchParams,
  resolveConversationIdFromUrl,
} from './lib/hooks/navigation';
import {
  buildHitlInterruptFromRaw,
  calculateDuration,
  canDeleteThisAIMessage,
  createHitlEditUserMessage,
  getInitialChatHistory,
  getModelSettings,
  getParticipantById,
  getSelectedConversationModel,
  getToolActionOriginalName,
  getWelcomeMessage,
} from './lib/chat.helpers';
import { extractFirstName, extractHumanReadableName, getChatUserSettings, setUserLLmSettings } from './lib/newConversation.helpers';

export type { Conversation} from './model/types';
export { hasPlaybackConversation} from './model/selectors';

/** Conversation + message CRUD (`../api/conversationApi.ts`, `../api/messageApi.ts`) — TanStack hooks (`use*`) and their underlying plain-async fetchers, bundled. */
export const conversationApi = {
  useCreate: useConversationCreateMutation,
  useEdit: useConversationEditMutation,
  useDelete: useDeleteConversationMutation,
  useDetails: useConversationDetailsQuery,
  useSelect: useSelectConversationMutation,
  useUnselect: useUnselectConversationMutation,
  useRegenerate: useRegenerateMutation,
  useStopTask: useStopChatTaskMutation,
  useMessageList: useMessageListQuery,
  useDeleteMessage: useDeleteMessageFromConversationMutation,
  useDeleteAllMessages: useDeleteAllMessagesFromConversationMutation,
  create: conversationCreate,
  edit: conversationEdit,
  remove: deleteConversation,
  details: conversationDetails,
  select: selectConversation,
  unselect: unselectConversation,
  regenerate,
  startAgentExecution,
  /**
   * The `execution_contract` values the Go agent-execution route admits
   * (issue #93). Bundled onto this namespace object rather than exported
   * as three more barrel symbols — §3.5's 20-symbol slice budget is already
   * exactly met, and these belong to `startAgentExecution` anyway.
   */
  contracts: {
    application: AGENT_EXECUTE_APPLICATION_CONTRACT,
    adhoc: AGENT_EXECUTE_ADHOC_CONTRACT,
    regenerate: AGENT_REGENERATE_CONTRACT,
    continueHitl: AGENT_CONTINUE_HITL_CONTRACT,
    continueAuthorization: AGENT_CONTINUE_AUTHORIZATION_CONTRACT,
    continueOutputLimit: AGENT_CONTINUE_OUTPUT_LIMIT_CONTRACT,
  },
  continueAgentExecution,
  stopTask: stopChatTask,
  messageList,
  deleteMessage: deleteMessageFromConversation,
  deleteAllMessages: deleteAllMessagesFromConversation,
} as const;

/** The 8 context-management endpoints (`../api/contextManagementApi.ts`), same hooks + fetchers bundling as `conversationApi`. */
export const contextManagementApi = {
  useGetStatus: useGetContextStatusQuery,
  useUpdateStrategy: useUpdateContextStrategyMutation,
  useOptimize: useOptimizeContextMutation,
  useGetAnalytics: useGetContextAnalyticsQuery,
  useGenerateSummary: useGenerateSummaryMutation,
  useGetSummaries: useGetConversationSummariesQuery,
  useUpdateSummary: useUpdateSummaryMutation,
  useDeleteSummary: useDeleteSummaryMutation,
  getStatus: getContextStatus,
  updateStrategy: updateContextStrategy,
  optimize: optimizeContext,
  getAnalytics: getContextAnalytics,
  generateSummary,
  getSummaries: getConversationSummaries,
  updateSummary,
  deleteSummary,
} as const;

export { useChatSessionStore } from './model/chatSessionStore';

export { useConversationLifecycle } from './lib/hooks/useConversationLifecycle';

/** Pure URL-target builders ported from `useConversationNavigation.hooks.js`/`useResetCreateFlag.js` — see `./lib/hooks/navigation.ts`'s module doc for why these are plain functions, not a router-calling hook. */
export const conversationNavigation = {
  resolveConversationIdFromUrl,
  buildUrlChange: buildConversationUrlChange,
  buildClearUrl: buildClearConversationUrl,
  buildCreateUrl: buildCreateConversationUrl,
  buildResetSearchParams,
} as const;

export { useChatStreaming } from './lib/hooks/useChatStreaming';
export { useAttachmentState } from './lib/hooks/useAttachmentState';
export { useUploadAttachments } from './lib/hooks/useUploadAttachments';

/** Pure helpers ported from `chat.helpers.js` (`./lib/chat.helpers.ts`) — welcome message, duration formatting, participant/model lookups, HITL interrupt shaping. */
export const chatHelpers = {
  getWelcomeMessage,
  getInitialChatHistory,
  calculateDuration,
  getParticipantById,
  canDeleteThisAIMessage,
  getToolActionOriginalName,
  buildHitlInterruptFromRaw,
  createHitlEditUserMessage,
  getSelectedConversationModel,
  getModelSettings,
} as const;

/** Pure helpers ported from `newConversation.helpers.js` (`./lib/newConversation.helpers.ts`). */
export const newConversationHelpers = {
  extractHumanReadableName,
  extractFirstName,
  getChatUserSettings,
  setUserLLmSettings,
} as const;

/**
 * Share a conversation by link. Exported from the slice's PUBLIC API because
 * `features/chat-conversation-list` owns the affordance and must not reach past
 * this barrel — `no-deep-slice-import-cross-slice` rejects that, and rightly:
 * a deep import binds a consumer to this slice's file layout.
 */
export {
  SHARED_CHAT_LINKS_QUERY_KEY,
  useCreateShareLinkMutation,
  useRevokeShareLinkMutation,
  useShareLinksQuery} from './api/sharedLinksApi';
export type { ShareLinkExpiry, SharedChatLink } from './api/sharedLinksApi';
