/**
 * An in-memory `RulesIpc` for tests and the dev harness. `put` validates
 * like the host does for what a test needs: a `match` example that does not
 * start with the rule's literal words is refused with the rule's 1-based
 * index; a `put` (or import) from a stale `version` (`digest`) is a
 * conflict, as on the host; `explain` answers from `answers` by command
 * (else the defaults). `touch` changes a file behind the panel's back.
 */
import type { PatternToken, RuleExplanation, RuleFile, RuleLoadError, RulePutOutcome, RulesIpc, RulesView, RuleScope } from './rulesIpc';

export interface FakeRulesIpc extends RulesIpc {
  readonly calls: { puts: { scope: RuleScope; file: RuleFile }[]; explained: string[]; imports: number };
  /** Replace `scope`'s file as another writer would (its version changes). */
  touch(scope: RuleScope, file: RuleFile): void;
}

const tokenMatches = (token: PatternToken, word: string | undefined): boolean => {
  if (typeof token === 'string') return word === token;
  if (Array.isArray(token)) return token.includes(word ?? '');
  return word !== undefined;
};

function firstBadExample(file: RuleFile, path: string): RuleLoadError | null {
  for (const [position, rule] of file.rules.entries()) {
    for (const example of rule.match ?? []) {
      const words = typeof example === 'string' ? example.split(/\s+/) : example;
      if (!rule.pattern.every((token, at) => tokenMatches(token, words[at]))) {
        const text = typeof example === 'string' ? example : example.join(' ');
        return { file: path, rule_index: position + 1, example: text, expected: 'match', got: 'did not match', message: `rule ${position + 1}: the match example \`${text}\` does not match it` };
      }
    }
  }
  return null;
}

const conflict: RulePutOutcome = { saved: false, file: null, error: null, conflict: true };

export function createFakeRulesIpc(initial: RulesView, answers: Record<string, RuleExplanation> = {}): FakeRulesIpc {
  let view = initial;
  let writes = 0;
  const calls: FakeRulesIpc['calls'] = { puts: [], explained: [], imports: 0 };
  const clone = (): RulesView => structuredClone(view);
  const replace = (scope: RuleScope, file: RuleFile): void => {
    writes += 1;
    view = { ...view, scopes: view.scopes.map((state) => (state.scope === scope ? { ...state, file: structuredClone(file), error: null, version: `v${writes}` } : state)) };
  };
  return {
    calls,
    touch: replace,
    get: () => Promise.resolve(clone()),
    put(_workspaceId, scope, file, version) {
      calls.puts.push({ scope, file: structuredClone(file) });
      const state = view.scopes.find((candidate) => candidate.scope === scope);
      if (state !== undefined && state.version !== version) return Promise.resolve(conflict);
      const error = firstBadExample(file, state?.path ?? scope);
      if (error !== null) return Promise.resolve<RulePutOutcome>({ saved: false, file: null, error, conflict: false });
      replace(scope, file);
      return Promise.resolve({ saved: true, file, error: null, conflict: false });
    },
    explain(_workspaceId, command) {
      calls.explained.push(command);
      return Promise.resolve(answers[command] ?? { verdict: 'allow', source: 'default', reason: 'a routine command in the sandbox', rule: null, justification: null });
    },
    importProposed(_workspaceId, digest) {
      calls.imports += 1;
      if (view.proposed?.digest !== digest) return Promise.resolve(conflict);
      const proposed = view.proposed.file?.rules ?? [];
      const current = view.scopes.find((state) => state.scope === 'workspace')?.file?.rules ?? [];
      replace('workspace', { version: 1, rules: [...current, ...proposed] });
      return Promise.resolve({ saved: true, file: null, error: null, conflict: false });
    },
  };
}
