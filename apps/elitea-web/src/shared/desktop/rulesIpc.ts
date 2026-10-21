/**
 * The typed client for the host's approval rules (IPC.md, "Approval rules";
 * #1216): the person's and the workspace's command prefix rules, what a
 * repository proposes, and "which layer would decide this command". Wire
 * shapes are the host's (snake_case). Failures reject with a
 * `WorkspaceIpcError`; a file that does not validate is an outcome
 * (`{saved: false, error}`), not a rejection, and so is a write made from a
 * stale read (`{saved: false, conflict: true}`): `put` names the `version`
 * it was made from, `importProposed` the proposal's `digest`.
 */
import type { HostInvoke } from './hostBridge';
import { tauriInvoke } from './hostBridge';
import { toWorkspaceIpcError } from './workspaceIpc';

export type RuleDecision = 'allow' | 'prompt' | 'forbidden';
/** A rules file's scope. */
export type RuleScope = 'workspace' | 'user';
/** A pattern element: a token, a list of alternatives, or any one word. */
export type PatternToken = string | string[] | { any: true };
/** An example: a command line, or argv. */
export type RuleExample = string | string[];

export interface PrefixRule {
  pattern: PatternToken[];
  decision?: RuleDecision;
  justification?: string;
  match?: RuleExample[];
  not_match?: RuleExample[];
  sandbox?: 'read-only' | 'workspace-write' | 'full-access';
  network?: boolean;
  /** Imported from a repository's proposal: its justification never reaches the model. */
  origin?: 'repository';
}

export interface RuleFile {
  version: 1;
  host_executables?: { name: string; paths: string[] }[];
  rules: PrefixRule[];
}

/** Why a file loads no rules (or a write was refused). `rule_index` is 1-based. */
export interface RuleLoadError {
  file: string;
  rule_index?: number;
  example?: string;
  expected?: 'match' | 'not_match';
  got?: string;
  message: string;
}

export interface RuleScopeState {
  scope: RuleScope;
  path: string;
  file: RuleFile | null;
  error: RuleLoadError | null;
  /** The file's digest when read; a `put` passes it back. */
  version: string;
}

export interface RuleProposal {
  path: string;
  count: number;
  /** The rules an import copies (pins dropped, allows bounded). */
  file: RuleFile | null;
  error: RuleLoadError | null;
  /** What an import leaves out or narrows. */
  warnings: string[];
  digest: string;
}

export interface RulesView {
  scopes: RuleScopeState[];
  proposed: RuleProposal | null;
}

export interface RulePutOutcome {
  saved: boolean;
  file: RuleFile | null;
  error: RuleLoadError | null;
  /** The file (or proposal) changed since it was read: nothing was written. */
  conflict: boolean;
}

export interface RuleExplanation {
  verdict: 'allow' | 'ask' | 'deny';
  source: 'policy' | 'plan_mode' | 'workspace' | 'user' | 'remembered' | 'default';
  reason: string;
  rule: { scope: RuleScope | 'legacy'; index: number; decision: RuleDecision; pattern: string; justification?: string; origin?: 'repository' } | null;
  justification: string | null;
}

export interface RulesIpc {
  get(workspaceId: string): Promise<RulesView>;
  put(workspaceId: string, scope: RuleScope, file: RuleFile, version: string): Promise<RulePutOutcome>;
  explain(workspaceId: string, command: string): Promise<RuleExplanation>;
  importProposed(workspaceId: string, digest: string): Promise<RulePutOutcome>;
}

function createRulesIpc(invoke: HostInvoke): RulesIpc {
  const call = <T>(command: string, args: Record<string, unknown>): Promise<T> =>
    invoke<T>(command, args).catch((error: unknown) => {
      throw toWorkspaceIpcError(error);
    });
  return {
    get: (workspaceId) => call<RulesView>('rules_get', { workspace_id: workspaceId }),
    put: (workspaceId, scope, file, version) => call<RulePutOutcome>('rules_put', { workspace_id: workspaceId, scope, file, version }),
    explain: (workspaceId, command) => call<RuleExplanation>('rules_explain', { workspace_id: workspaceId, command }),
    importProposed: (workspaceId, digest) => call<RulePutOutcome>('rules_import_proposed', { workspace_id: workspaceId, digest }),
  };
}

/** The real client, or `undefined` outside the Tauri webview. */
export function tauriRulesIpc(): RulesIpc | undefined {
  const invoke = tauriInvoke();
  return invoke === undefined ? undefined : createRulesIpc(invoke);
}
