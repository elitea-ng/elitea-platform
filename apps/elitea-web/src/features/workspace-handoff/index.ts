/**
 * Public API — the desktop-only cloud handoff UI (ADR-0034): send a Local
 * work thread to the cloud executor and take it back. Dialogs, the banner
 * and badge of a thread that is not running here, the status hook, and where
 * they get the host client. Reached only from `pages/workspace` and the
 * desktop shell, which are themselves reached only in the `desktop` build.
 */
export { CloudThreadBanner } from './ui/CloudThreadBanner';
export { ExecutorBadge } from './ui/ExecutorBadge';
export { EraseCloudCopyDialog } from './ui/EraseCloudCopyDialog';
export { SendToCloudDialog } from './ui/SendToCloudDialog';
export { HandoffSettingsDialog } from './ui/HandoffSettingsDialog';
export { TakeBackDialog } from './ui/TakeBackDialog';
export { HandoffIpcProvider, useHandoffIpc } from './model/ipcContext';
export { handoffStatusKey, handoffStatusManyKey, isOffLaptop, useHandoffStatus, useHandoffStatusMany } from './model/useHandoffStatus';
export { useTakeBackRequests } from './model/takeBackRequests';
export { describeHandoffError } from './model/describeHandoffError';
export { readThreadUuids, rememberThreadUuid } from './model/threadUuids';
