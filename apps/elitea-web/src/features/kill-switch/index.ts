/**
 * Public API — spec §3.3: named exports only.
 *
 * The kill switch (ADR-0041 decision 1): the agent page's Stop agent action,
 * the suspension wire shape and the refusal words the admin console reuses.
 */
export { StopAgentButton } from './ui/StopAgentButton';
export {
  SAFETY_SUSPEND_PERMISSION,
  describeSuspensionError,
  suspensionRow,
  suspensionRows,
  type ExecutionSuspensionRow,
} from './api/suspensions';
