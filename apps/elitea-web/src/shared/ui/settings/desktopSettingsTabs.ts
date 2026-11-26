/**
 * The desktop app's own Settings tabs (#3078): the one list the drawer's rows
 * (`settingsSections.ts`), the `/settings/$tab` catch-all that renders them
 * (`routes/_shell/settings/$tab.tsx`) and the unknown-tab redirect
 * (`SettingsRedirect.tsx`) read, so a new desktop tab cannot be drawn but
 * bounced again.
 *
 * They ride on the catch-all instead of route files of their own so the web
 * build's route tree does not grow. They are tabs only in the desktop build
 * (`MODE` is a build-time literal): in the web build the slugs are unknown and
 * keep redirecting.
 *
 * - `approval-rules`: the approval rules panel (#1216);
 * - `mcp-servers`: the local MCP servers (#1230);
 * - `hooks`: the person's hooks (#1237);
 * - `computer-use`: computer use on this device (ADR-0047, #1584);
 * - `trusted-folders`: the folders whose commands run without a sandbox (ADR-0061, #3062);
 * - `troubleshoot`: the Doctor (apps/elitea-desktop README, "Diagnostics").
 */
export const DESKTOP_SETTINGS_TABS = [
  { id: 'approval-rules', label: 'Approval rules' },
  { id: 'mcp-servers', label: 'MCP servers' },
  { id: 'hooks', label: 'Hooks' },
  { id: 'computer-use', label: 'Computer use' },
  { id: 'trusted-folders', label: 'Trusted folders' },
  { id: 'troubleshoot', label: 'Troubleshoot' },
] as const;

export type DesktopSettingsTab = (typeof DESKTOP_SETTINGS_TABS)[number]['id'];

/** The build is the desktop app (`mode` defaults to this build's). */
function isDesktopBuild(mode: string = import.meta.env.MODE): boolean {
  return mode === 'desktop';
}

/** `tab` is one of the desktop tabs, in a desktop build (`mode` defaults to this build's). */
export function isDesktopSettingsTab(tab: string, mode: string = import.meta.env.MODE): tab is DesktopSettingsTab {
  return isDesktopBuild(mode) && DESKTOP_SETTINGS_TABS.some((entry) => entry.id === tab);
}
