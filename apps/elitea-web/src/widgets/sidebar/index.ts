/**
 * Public API — spec §3.3 (named exports only). Unit W-shell (spec §9.3).
 *
 * Owns: the app's left-hand navigation sidebar — 3 permission-gated nav
 * groups (SHELL-001..010), the project switcher, the live-connection
 * dot (SHELL-012), settings/help-center footer links. Ported from
 * `[fsd]/widgets/sidebar-root/**`, `[fsd]/app/layout/{MainSidebar,
 * MainPanel}.jsx`.
 *
 * Consumed by `widgets/app-shell`, the only intended caller — `<Sidebar>`
 * needs a `projectId`/`projects`/`permissions` triple that only makes sense
 * once a page is mounted inside `<AppShell>`.
 *
 * Known gaps, each documented at its source file (not silently dropped):
 *  - No "current user identity" (id, personal_project_id, name/avatar) —
 *    no landed Wave-1 unit exposes a React-reachable session/profile hook
 *    (`shared/api/generated/**` has no "current user" endpoint; F4's
 *    `verifySession` is a boolean probe, not a data hook). Every place the
 *    old app branched on `personal_project_id`
 *    (`useDisablePersonalSpace`, "no personal project yet -> redirect to
 *    Onboarding on nav click") has no equivalent here.
 *  - The Feedback FAB (SHELL-027, PERM-051, ACT-087; waiver W-007 "ship
 *    dormant feature") is NOT implemented: its submit action
 *    (`useFeedbackMutation`, old app's `api/social.js`) has no generated or
 *    hand-registered endpoint anywhere in `shared/api/generated/**` — no
 *    "social"/"feedback" tag was generated, and this widget cannot add a
 *    hand-written one to `shared/api/endpoints/` (outside its ownership
 *    fence, §R-A5). Building the dialog UI without a working submit path
 *    would be exactly the "no placeholder code" rule's target.
 *  - `NotificationButton` is NOT ported: `useNotificationListQuery`
 *    (old app's `api/notifications.js`) has no generated endpoint either
 *    (no "notifications" REST tag; only the `notifications_notify` SOCKET
 *    event exists, S5), and the notification LIST panel
 *    (`[fsd]/widgets/Notifications/ui`) is outside this unit's owned paths.
 */
export { Sidebar, SIDE_BAR_WIDTH_PX, COLLAPSED_SIDE_BAR_WIDTH_PX } from './ui/Sidebar';
export type { SidebarProps } from './ui/Sidebar';
export { ProjectAvatar } from './ui/ProjectAvatar';
export { useSidebarCollapsedStore } from './model/sidebarCollapsed.store';
export { usePermissionSet } from './api/usePermissionSet';
export { useProjectOptions } from './api/useProjectOptions';
export type { ProjectOptionsResult } from './api/useProjectOptions';
export { orderedProjectOptions } from './lib/projectOptions';
export {
  navSections,
  selectedNavItem,
  visibleNavSections,
  type NavItem,
  type NavItemValue,
  type NavSection,
} from './lib/navSections';
