import { isEndingSession, isSuspendingWorkspace } from '@/security/endSession';
import { boundWorkspace } from '@/security/secureStorage';

/**
 * Whether something of `workspaceId` may be written to a system surface (the
 * widget snapshot, the Siri agent catalog) now: only while that workspace is
 * bound and is not being ended or put aside for another one.
 *
 * The wipe hooks themselves re-render the signed-in layout (live turns
 * cleared, the store closed) while the session still names the old
 * workspace, and its effects then republish what the hook before had just
 * cleared. After the session resets the layout is unmounted, so nothing
 * would clear it again. Clearing (`null`) is always allowed.
 */
export function maySurfaceWorkspace(workspaceId: string | null): boolean {
  if (workspaceId === null) return true;
  return boundWorkspace() === workspaceId && !isEndingSession() && !isSuspendingWorkspace();
}
