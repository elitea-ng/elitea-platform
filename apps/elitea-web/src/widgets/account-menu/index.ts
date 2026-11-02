/**
 * Public API — the account menu at the sidebar's foot: the user app's
 * (web and desktop) and the frame the admin console builds its own from.
 */
export { AccountMenu, type AccountMenuItem, type AccountMenuProps } from './ui/AccountMenu';
export { AccountMenuFrame, type AccountIdentity, type AccountMenuFrameProps } from './ui/AccountMenuFrame';
export { versionLine } from './lib/versionLine';
export { useServerVersion } from './model/useServerVersion';
