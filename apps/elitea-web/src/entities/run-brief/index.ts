/**
 * Public API — spec §3.3: named exports only.
 *
 * The run brief and the goal check of an unattended chain (ADR-0045
 * decision 11) as the transcripts show them: Main's `meta.run_brief` and
 * `meta.goal_check` in the cloud, the host's `run_brief` and `goal_checked`
 * events on the desktop.
 */
export { goalCheckOfChainEnd, readGoalCheck, readRunBrief, type GoalCheckView, type RunBriefView } from './model/runBrief';
export { RunBriefCard } from './ui/RunBriefCard';
export { GoalCheckNotice } from './ui/GoalCheckNotice';
