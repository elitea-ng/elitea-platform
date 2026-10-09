/**
 * Public API — the desktop app's workspace-first frame (ADR-0029). Reached
 * only from `widgets/app-shell` (a lazy import behind a literal
 * `import.meta.env.MODE === 'desktop'`, so the web build drops it) and from
 * the desktop-only `pages/workspace`.
 */
export { DesktopFrame, ShowSidebarButton } from './ui/DesktopFrame';
export type { DesktopFrameProps } from './ui/DesktopFrame';
export { TitleBarSpacer } from './ui/TitleBarSpacer';
export { useDesktopLayout, CHANGES_WIDTH } from './model/desktopLayout.store';
export type { SessionActions } from './model/desktopLayout.store';
export { modKey } from './ui/shortcutLabel';
export { AppIpcProvider, useAppIpc } from './model/appIpcContext';
export { DoctorPanel } from './ui/DoctorPanel';
export type { DoctorPanelProps } from './ui/DoctorPanel';
