/**
 * widgets/context-budget — the "Context Budget" panel at the foot of the chat
 * participants rail, and the editor its pencil opens. See
 * `ui/ContextBudget.tsx` for the mounting contract, what the pencil writes,
 * and the one part of the reference's modal that is still not built.
 */
export { ContextBudget } from './ui/ContextBudget';
export type { ContextBudgetProps } from './ui/ContextBudget';
export { ContextBudgetPanel } from './ui/ContextBudgetPanel';
export type { ContextBudgetPanelProps } from './ui/ContextBudgetPanel';
export { toContextBudgetStats, formatNumberWithSpaces } from './lib/contextStatus';
export type { ContextBudgetStats } from './lib/contextStatus';
export {
  buildContextBudgetUpdate,
  readContextBlock,
  selectMaxContextTokens,
  validateMaxContextTokens,
  MAX_MAX_CONTEXT_TOKENS,
  MIN_MAX_CONTEXT_TOKENS,
} from './lib/authorContextUpdate';
export type { AuthorContextProfile } from './lib/authorContextUpdate';
