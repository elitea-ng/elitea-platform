/**
 * Which state changes a person may make on a listing version (ADR-0051
 * decision 2). The publisher may deprecate, yank and restore. A platform
 * administrator may yank, and restore a yanked version. The server decides again; this only hides a button
 * that could end in a 403.
 */
import type { MarketplaceVersion } from '@/shared/api/generated/model';

/** The state a change sets: the same words the route takes. */
export type VersionAction = 'deprecated' | 'yanked' | 'available';

export function versionActions(version: MarketplaceVersion, canManage: boolean, canModerate: boolean): VersionAction[] {
  const actions: VersionAction[] = [];
  const open = version.state === 'current' || version.state === 'available';
  if (canManage && open) actions.push('deprecated');
  if ((canManage || canModerate) && version.state !== 'yanked') actions.push('yanked');
  // An administrator may restore a version that was YANKED (not one the publisher deprecated).
  if (!open && (canManage || (canModerate && version.state === 'yanked'))) actions.push('available');
  return actions;
}
