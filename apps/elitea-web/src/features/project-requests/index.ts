/**
 * Public API — spec §3.3: named exports only.
 *
 * Self-service "request a project" (#871). `widgets/sidebar`'s
 * `ProjectSwitcher` renders the trigger row; this slice owns the dialog,
 * the submit mutation and the "your requests" read.
 */
export { RequestProjectDialog } from './ui/RequestProjectDialog';
