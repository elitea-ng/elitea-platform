/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 *
 * The agent editor's SKILLS section: read, attach and detach the skills one
 * agent VERSION carries. A slice of its own rather than surface on
 * `features/skills`, whose barrel is at its budget, and rather than on
 * `features/agents`, which is also at 20/20 — and because this is the one
 * place the two domains meet, so a slice named after the join is what the
 * layer rules allow (`no-sideways-features`).
 */
export { AgentSkillsPanel } from './ui/AgentSkillsPanel';
export { useAgentSkills, useSkillPicker, agentSkillKeys } from './model/useAgentSkills';
export type { AgentSkillsState } from './model/useAgentSkills';
export { MAX_SKILLS_PER_AGENT } from './api/agentSkillsApi';
export type { AgentSkill } from './api/agentSkillsApi';
