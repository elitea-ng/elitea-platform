/**
 * Public API — spec §3.3: named exports only.
 *
 * A project's Send to cloud switch (ADR-0034 decision 11): the shared
 * control both pages render, the project settings section, and the error
 * wording. `pages/settings/ProjectGeneral` renders the section;
 * `pages/admin`'s Projects page renders the control in its own dialog over
 * the admin route.
 */
export { CloudExecutorSwitchControl } from './ui/CloudExecutorSwitchControl';
export { ProjectCloudExecutorSection } from './ui/ProjectCloudExecutorSection';
export { describeCloudExecutorSwitchError } from './ui/describeCloudExecutorSwitchError';
