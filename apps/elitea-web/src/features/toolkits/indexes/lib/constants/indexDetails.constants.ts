/**
 * Port of `apps/elitea-ui/src/[fsd]/features/toolkits/indexes/lib/constants/
 * indexDetails.constants.js` (unit A4a). Byte-for-byte value port of the
 * enums/labels — the baseline has no logic here, only lookup tables.
 *
 * Also carries a small, LOCALLY-SCOPED subset of `apps/elitea-ui/src/common/
 * constants.js`'s `SocketMessageType`/`PERMISSIONS`/`SearchParams` — only the
 * members this sub-unit's owned files actually read. `common/constants.js`
 * (970+ lines, dozens of unrelated enums for chat/applications/analytics/
 * etc.) has no promoted `shared/`/`entities/` home for THESE THREE in this
 * app yet (confirmed: `shared/lib/enums.ts` (S3) carries only `ROLES`/
 * `WELCOME_MESSAGE_ID`/a handful of others; grep confirmed neither
 * `SocketMessageType` nor `PERMISSIONS` nor `SearchParams` exist anywhere
 * under `src/shared/` or `src/entities/`). Duplicating the few members
 * needed — rather than the whole file — matches this program's established
 * "port it yourself, locally, inside your own owned files" precedent (see
 * e.g. `features/agents/api/useSelectedProjectId.ts`'s own doc comment for
 * the same class of decision applied to a hook instead of a constants
 * table).
 *
 * `ChatParticipantType`/`ToolActionStatus`, by contrast, ARE already
 * promoted — `shared/lib/chat.ts` (unit S3) carries the full, real ports;
 * this file re-exports nothing for them and callers import directly from
 * there instead (see `indexChat.helpers.ts`).
 */
import { FRAME_TYPE } from '@elitea/client-core/stream';

export const NEW_INDEX_ID = 'new_index';

export const IndexViewsEnum = {
  create: 'create',
  edit: 'edit',
} as const;

export const EditViewTabsEnum = {
  run: 'run',
  configuration: 'configuration',
  history: 'history',
} as const;

export const IndexesToolsEnum = {
  indexData: 'index_data',
  searchIndexData: 'search_index',
  stepbackSearchIndex: 'stepback_search_index',
  stepbackSummaryIndex: 'stepback_summary_index',
  removeIndex: 'remove_index',
} as const;

export const IndexStatuses = {
  progress: 'in_progress',
  success: 'completed',
  fail: 'failed',
  cancelled: 'cancelled',
  created: 'created',
  partlyOk: 'partly_indexed',
} as const;

/** Statuses that allow the index to be searched and run tools against. */
export const RUNNABLE_INDEX_STATUSES: readonly string[] = [IndexStatuses.success, IndexStatuses.partlyOk];

export const IndexHistoryItemsLabels: Readonly<Record<string, string>> = {
  [IndexStatuses.success]: 'Reindexed',
  [IndexStatuses.created]: 'Created',
  [IndexStatuses.cancelled]: 'Stopped',
  [IndexStatuses.fail]: 'Failed',
  [IndexStatuses.partlyOk]: 'Partially Indexed',
};

export const IndexCronDefault = '0 0 * * 6';

/**
 * `common/constants.js:157-192`'s `SocketMessageType` (`@elitea/client-core`'s
 * `FRAME_TYPE`), trimmed to the members `indexChat.helpers.ts`'s
 * `generateChatMessageBasedOnResponse` switch statement actually reads.
 */
export const SocketMessageType = {
  AgentToolStart: FRAME_TYPE.AgentToolStart,
  AgentToolEnd: FRAME_TYPE.AgentToolEnd,
  AgentToolError: FRAME_TYPE.AgentToolError,
  AgentException: FRAME_TYPE.AgentException,
  Chunk: FRAME_TYPE.Chunk,
  AIMessageChunk: FRAME_TYPE.AIMessageChunk,
  StartTask: FRAME_TYPE.StartTask,
  Error: FRAME_TYPE.Error,
  AgentThinkingStep: FRAME_TYPE.AgentThinkingStep,
  AgentThinkingStepUpdate: FRAME_TYPE.AgentThinkingStepUpdate,
  AgentResponse: FRAME_TYPE.AgentResponse,
} as const;

/** `common/constants.js:604-606`'s `PERMISSIONS.index`, the only branch this sub-unit reads. */
export const PERMISSIONS = {
  index: {
    schedule: 'models.applications.index_meta.edit',
  },
} as const;

/** `common/constants.js:295`'s `SearchParams.ToolkitType` sibling — the one member `IndexesContainer.tsx` reads (the "open this index from a notification link" query param). */
export const SearchParams = {
  IndexName: 'index_name',
} as const;
