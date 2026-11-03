/** The thread's checklist across its turns. */
import type { PlanView } from '@/shared/desktop/planIpc';

/** The newest `plan` any of `views` (oldest first) sent; `null` when none did or it was deleted. */
export function latestPlan(views: readonly { plan?: PlanView | null }[]): PlanView | null {
  for (let i = views.length - 1; i >= 0; i -= 1) {
    const plan = views[i]?.plan;
    if (plan !== undefined) return plan;
  }
  return null;
}
