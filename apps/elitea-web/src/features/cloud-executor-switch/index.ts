/**
 * Public API — spec §3.3: named exports only.
 *
 * A project's Send to cloud switch (ADR-0034 decision 11): the shared
 * control and the error wording. `pages/admin`'s Projects page renders the
 * control in its own dialog over the admin route.
 */
export { CloudExecutorSwitchControl } from './ui/CloudExecutorSwitchControl';
export { describeCloudExecutorSwitchError } from './ui/describeCloudExecutorSwitchError';
