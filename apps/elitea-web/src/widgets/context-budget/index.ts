/**
 * widgets/context-budget — the "Context Budget" panel at the foot of the chat
 * participants rail, and the editor its pencil opens. See
 * `ui/ContextBudget.tsx` for the mounting contract, what the pencil writes,
 * and the one part of the reference's modal that is still not built.
 */
export { ContextBudget } from './ui/ContextBudget';
export type { ContextBudgetProps } from './ui/ContextBudget';
export { ContextBudgetPanel } from './ui/ContextBudgetPanel';
export { AgentContextMeter } from './ui/AgentContextMeter';
export type { AgentContextMeterProps } from './ui/AgentContextMeter';
export { readAgentContexts } from './lib/agentContexts';
export type { AgentContextMeter as AgentContextMeterStats } from './lib/agentContexts';
export type { ContextBudgetPanelProps } from './ui/ContextBudgetPanel';
export { toContextBudgetStats, formatNumberWithSpaces } from './lib/contextStatus';
export type { ContextBudgetStats } from './lib/contextStatus';
export {
  buildContextBudgetUpdate,
  readContextBlock,
  selectBudgetMode,
} from './lib/authorContextUpdate';
export type { AuthorContextProfile } from './lib/authorContextUpdate';
