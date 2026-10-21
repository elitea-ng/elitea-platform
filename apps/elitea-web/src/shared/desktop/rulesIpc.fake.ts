/**
 * An in-memory `RulesIpc` for tests and the dev harness. `put` validates
 * like the host does for what a test needs: a `match` example that does not
 * start with the rule's literal words is refused with the rule's 1-based
 * index; `explain` answers from `answers` by command (else the defaults).
 */
import type { RuleExplanation, RuleFile, RuleLoadError, RulePutOutcome, RulesIpc, RulesView, RuleScope } from './rulesIpc';

export interface FakeRulesIpc extends RulesIpc {
  readonly calls: { puts: { scope: RuleScope; file: RuleFile }[]; explained: string[]; imports: number };
}

function firstBadExample(file: RuleFile, path: string): RuleLoadError | null {
  for (const [position, rule] of file.rules.entries()) {
    for (const example of rule.match ?? []) {
      const words = typeof example === 'string' ? example.split(/\s+/) : example;
      const matches = rule.pattern.every((token, at) => (typeof token === 'string' ? words[at] === token : token.includes(words[at] ?? '')));
      if (!matches) {
        const text = typeof example === 'string' ? example : example.join(' ');
        return { file: path, rule_index: position + 1, example: text, expected: 'match', got: 'did not match', message: `rule ${position + 1}: the match example \`${text}\` does not match it` };
      }
    }
  }
  return null;
}

export function createFakeRulesIpc(initial: RulesView, answers: Record<string, RuleExplanation> = {}): FakeRulesIpc {
  let view = initial;
  const calls: FakeRulesIpc['calls'] = { puts: [], explained: [], imports: 0 };
  const clone = (): RulesView => structuredClone(view);
  return {
    calls,
    get: () => Promise.resolve(clone()),
    put(_workspaceId, scope, file) {
      calls.puts.push({ scope, file: structuredClone(file) });
      const path = view.scopes.find((state) => state.scope === scope)?.path ?? scope;
      const error = firstBadExample(file, path);
      if (error !== null) return Promise.resolve<RulePutOutcome>({ saved: false, file: null, error });
      view = { ...view, scopes: view.scopes.map((state) => (state.scope === scope ? { ...state, file: structuredClone(file), error: null } : state)) };
      return Promise.resolve({ saved: true, file, error: null });
    },
    explain(_workspaceId, command) {
      calls.explained.push(command);
      return Promise.resolve(answers[command] ?? { verdict: 'allow', source: 'default', reason: 'a routine command in the sandbox', rule: null, justification: null });
    },
    importProposed() {
      calls.imports += 1;
      const proposed = view.proposed?.file?.rules ?? [];
      view = {
        ...view,
        scopes: view.scopes.map((state) => (state.scope === 'workspace' ? { ...state, file: { version: 1, rules: [...(state.file?.rules ?? []), ...proposed] }, error: null } : state)),
      };
      return Promise.resolve({ saved: true, file: null, error: null });
    },
  };
}
