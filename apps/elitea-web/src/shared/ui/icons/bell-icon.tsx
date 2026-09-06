/**
 * Bell outline — the Settings › Notifications nav item's icon.
 *
 * Baseline: `apps/elitea-ui/src/components/Icons/BellIcon.jsx`, which the
 * baseline's `SettingsDrawer` maps to the `notifications` tab. Every other
 * settings tab already had its baseline icon in this directory; this one did
 * not, so the drawer fell back to the generic gear for it.
 *
 * @public Icon surface: consumed by `shared/ui/settings/SettingsDrawer`.
 */
export { default as BellIcon } from './svg/bell-icon.svg?react';
