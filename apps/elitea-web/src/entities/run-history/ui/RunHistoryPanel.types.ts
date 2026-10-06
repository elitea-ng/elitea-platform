/** `entity_name` the run-history filter reads — see `listConversations`'s own description in v2.yaml. */
export type RunHistoryEntityName = 'application' | 'toolkit';

export interface RunHistoryPanelProps {
  readonly projectId: string | undefined;
  readonly editorTest?: boolean;
  readonly entityName: RunHistoryEntityName;
  readonly entityId: string | number | undefined;
  readonly onClose: () => void;
  readonly onRestoreConversation?: (conversationId: string) => void;
  /** #955 — this entity's own versions, for naming which one produced each run. Optional; see `RunHistoryList`'s own doc comment. */
  readonly versions?: readonly { readonly id: string | number; readonly name: string }[] | undefined;
}
