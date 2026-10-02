/** Product presets; the worker resolves capacities against the admitted model. */
export type ContextBudgetMode = 'balanced' | 'full';

export function resolveContextBudgetMode(value: unknown): ContextBudgetMode {
  return value === 'full' ? 'full' : 'balanced';
}
