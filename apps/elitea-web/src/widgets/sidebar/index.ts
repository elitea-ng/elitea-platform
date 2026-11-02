/**
 * Public API — the app's left-hand navigation rail (SHELL-001..012): the
 * permission-gated places, the project switcher, the Chats, the bell, the
 * settings/catalog/help links. Consumed by `widgets/app-shell`; the desktop
 * sidebar (`widgets/desktop-shell`) reuses its bell, its create gate and
 * its place list.
 */
export { Sidebar, SIDE_BAR_WIDTH_PX, COLLAPSED_SIDE_BAR_WIDTH_PX } from './ui/Sidebar';
export type { SidebarProps } from './ui/Sidebar';
export { ProjectAvatar } from './ui/ProjectAvatar';
export { useSidebarCollapsedStore } from './model/sidebarCollapsed.store';
export { NotificationBell } from './ui/NotificationButton';
export { useCreateGate } from './model/useCreateGate';
export { usePermissionSet } from './api/usePermissionSet';
export { useProjectOptions } from './api/useProjectOptions';
export type { ProjectOptionsResult } from './api/useProjectOptions';
export { orderedProjectOptions } from './lib/projectOptions';
export {
  computeIsSelectedProjectPublic,
  navSections,
  selectedNavItem,
  visibleNavSections,
  type NavItem,
  type NavItemValue,
  type NavSection,
} from './lib/navSections';
