/**
 * The thread-session shapes shared by the session hook (useThreadSession) and
 * the side panel (SessionPanel). They live here so neither file imports the
 * other.
 */
import type { ApprovalDecision, ChangedFile } from '@/shared/desktop/workspaceIpc';

/** An approval of this thread already decided, as the panel lists it. */
export interface DecidedApproval {
  requestId: string;
  title: string;
  decision: ApprovalDecision;
}

/** One turn's changes in the panel: live (the host still keeps it: diff from disk, undo) or as recorded when it ended (`files`). */
export interface ChangeSet {
  turnId: string;
  /** The prompt that started the turn. */
  label: string;
  /** Set for a turn the host no longer keeps: shown as recorded, without undo. */
  files?: ChangedFile[];
}
