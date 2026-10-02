/**
 * Context-budget constants — copied from the old-app's widget slice.
 *
 * Source: `apps/elitea-ui/src/[fsd]/widgets/context-budget/lib/constants.js`
 *
 * Copied here (not imported from `widgets/`) because:
 * - The `widgets/` directory does not exist in the new-app yet
 * - These constants are consumed by `pages/settings/` components,
 *   and `pages/` importing from `widgets/` would be an architectural
 *   violation once the widget is ported
 * - The constants are small, stable, and self-contained
 */

/** Field-name separator used in model value strings. */
export const SEPARATOR = '$$$';

/** Default context strategy configuration. */
export const DEFAULT_CONTEXT_STRATEGY = {
  ENABLED: true,
  PRESERVE_RECENT_MESSAGES: 5,
  PRESERVE_SYSTEM_MESSAGES: true,
  ENABLE_SUMMARIZATION: true,
  SYSTEM_MESSAGES: '',
};

/** Validation limits — used by Yup schemas. */
export const VALIDATION_LIMITS = {
  PRESERVE_RECENT_MESSAGES: {
    MIN: 1,
    MAX: 99,
  },
  MAX_TOKENS: {
    MIN: 100,
    MAX: 4096,
  },
};

/** Messages used in the context-budget UI. */
export const CONTEXT_MESSAGES = {
  HIGH_USAGE_WARNING: 'Context usage is high. Consider configuring budget settings.',
  SUMMARY_GUIDANCE_PLACEHOLDER:
    'Optional: emphasize decision reasons, deliverables, and unresolved risks.',
  SUMMARY_CONTRACT_DESCRIPTION:
    'Elitea always preserves the task, constraints, key facts, decisions, completed work, open issues, next steps, and verified references in a structured summary. Your guidance only adds emphasis.',
};
