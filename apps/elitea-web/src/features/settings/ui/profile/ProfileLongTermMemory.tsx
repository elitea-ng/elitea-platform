/**
 * ProfileLongTermMemory — was a dimmed, non-interactive "Coming soon"
 * placeholder (the exact evidence #870 cited). The real feature now lives in
 * `features/settings/ui/memory/LongTermMemoryManagement.tsx`, mounted here
 * unchanged so this dead route (`/settings/personalization`, no longer
 * linked from the settings nav — see `routes/_shell/settings/
 * settingsSections.ts`, which lists only profile/ai-personality/memory) does
 * not keep showing a placeholder that flatly contradicts the live
 * Settings > Memory tab a few clicks away.
 *
 * `projectId` is threaded down from the page (`pages/settings/
 * Personalization.tsx` → `ProfileFormContent` → `ProfileContextManagement`
 * → here) rather than read from `widgets/app-shell`'s selected-project
 * store directly — `features/` may not import `widgets/` (R-L1,
 * `.dependency-cruiser.cjs`'s `no-upward-from-features` rule).
 */
import { LongTermMemoryManagement } from '../memory/LongTermMemoryManagement';

export interface ProfileLongTermMemoryProps {
  readonly projectId?: string | undefined;
}

export function ProfileLongTermMemory({ projectId }: ProfileLongTermMemoryProps) {
  return <LongTermMemoryManagement projectId={projectId} />;
}
