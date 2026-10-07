/**
 * A turn the workspace refused because a model budget is exhausted.
 *
 * The server reports it as the run's `execution.failed` frame (and, on the
 * stored answer, `meta.error_code` next to `is_error`) with one of three
 * public codes (elitea-main runtimegrpc/output server.go, #6732):
 * - `MEMBER_BUDGET_EXHAUSTED`: the caller's own budget in the project;
 * - `PROJECT_BUDGET_EXHAUSTED`: the project's shared budget;
 * - `MODEL_BUDGET_EXHAUSTED`: a worker that could not tell the scope.
 *
 * The app shows its own plain message for each (not a raw provider error)
 * and offers the usage card, which names whose budget ran out.
 */

export type BudgetScope = 'member' | 'project' | 'unknown';

const CODES = new Map<string, BudgetScope>([
  ['MEMBER_BUDGET_EXHAUSTED', 'member'],
  ['PROJECT_BUDGET_EXHAUSTED', 'project'],
  ['MODEL_BUDGET_EXHAUSTED', 'unknown'],
]);

const MESSAGES: Record<BudgetScope, string> = {
  member: 'Your budget in this project is used up for this month. Ask a project admin to raise your limit, or wait until it resets.',
  project: "This project's shared budget is used up for this month. Ask a project admin to raise the limit, or wait until it resets.",
  unknown: 'The model budget is used up. Ask a project admin to check the budget before trying again.',
};

/** The budget a failure code names, or null for any other failure. */
export function budgetScopeOf(code: unknown): BudgetScope | null {
  return typeof code === 'string' ? (CODES.get(code) ?? null) : null;
}

export function budgetRefusalText(scope: BudgetScope): string {
  return MESSAGES[scope];
}
