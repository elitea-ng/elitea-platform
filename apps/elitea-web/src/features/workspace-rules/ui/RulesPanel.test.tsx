import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import type { RulesView } from '@/shared/desktop/rulesIpc';
import { createFakeRulesIpc } from '@/shared/desktop/rulesIpc.fake';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { RulesPanel } from './RulesPanel';

const VIEW: RulesView = {
  scopes: [
    {
      scope: 'workspace',
      path: '/data/workspaces/w1/rules.json',
      file: {
        version: 1,
        rules: [{ pattern: ['npm', 'publish'], decision: 'forbidden', justification: 'Publishing goes through the release pipeline; open a PR instead.', match: ['npm publish --tag next'] }],
      },
      error: null,
    },
    { scope: 'user', path: '/config/rules.json', file: { version: 1, rules: [{ pattern: ['cargo', ['test', 'check']], match: ['cargo test -p x'], not_match: ['cargo publish'] }] }, error: null },
  ],
  proposed: { path: '/repo/.elitea/rules.json', count: 1, file: { version: 1, rules: [{ pattern: ['make', 'test'] }] }, error: null },
};

describe('RulesPanel', () => {
  it('lists both files with decision, pattern, justification and examples, and the proposal', async () => {
    renderWithTheme(<RulesPanel ipc={createFakeRulesIpc(VIEW)} workspaceId="w1" />);
    const forbidden = await screen.findByTestId('rule-workspace-1');
    expect(forbidden).toHaveTextContent('Forbidden');
    expect(forbidden).toHaveTextContent('npm publish');
    expect(forbidden).toHaveTextContent('open a PR instead');
    expect(forbidden).toHaveTextContent('1 examples checked');
    const user = screen.getByTestId('rule-user-1');
    expect(user).toHaveTextContent('Allow');
    expect(user).toHaveTextContent('cargo [test|check]');
    expect(user).toHaveTextContent('2 examples checked');
    expect(screen.getByTestId('rules-proposed')).toHaveTextContent('This repository proposes 1 rules');
    expect(screen.getByText(/Forbidden rules are advisory/)).toBeInTheDocument();
  });

  it('shows why a file loads no rules', async () => {
    const broken: RulesView = {
      ...VIEW,
      scopes: [{ scope: 'workspace', path: '/data/rules.json', file: null, error: { file: '/data/rules.json', rule_index: 2, example: 'git status', expected: 'match', got: 'did not match', message: 'rule 2 (`git push`): the match example `git status` does not match it' } }, VIEW.scopes[1]!],
    };
    renderWithTheme(<RulesPanel ipc={createFakeRulesIpc(broken)} workspaceId="w1" />);
    const error = await screen.findByTestId('rules-error');
    expect(error).toHaveTextContent('Rule 2 is refused, so none of these rules are used');
    expect(within(error).getByTestId('rules-error-example')).toHaveTextContent('Example `git status`: expected a match, but it did not match.');
  });

  it('renders a validation error on save and writes nothing; a valid rule is saved', async () => {
    const ipc = createFakeRulesIpc(VIEW);
    const user = userEvent.setup();
    renderWithTheme(<RulesPanel ipc={ipc} workspaceId="w1" />);
    const workspace = await screen.findByTestId('rules-scope-workspace');
    await user.click(within(workspace).getByRole('button', { name: 'Add rule' }));
    await user.type(within(workspace).getByLabelText(/Command pattern/), 'git reset');
    await user.type(within(workspace).getByLabelText(/Examples it must match/), 'git status');
    await user.click(within(workspace).getByRole('button', { name: 'Save rule' }));
    expect(await within(workspace).findByTestId('rules-error')).toHaveTextContent('Rule 2 is refused');
    expect(within(workspace).queryByTestId('rule-workspace-2')).toBeNull();

    await user.clear(within(workspace).getByLabelText(/Examples it must match/));
    await user.type(within(workspace).getByLabelText(/Examples it must match/), 'git reset --soft HEAD~1');
    await user.click(within(workspace).getByRole('button', { name: 'Save rule' }));
    expect(await screen.findByTestId('rule-workspace-2')).toHaveTextContent('git reset');
    expect(ipc.calls.puts.at(-1)?.file.rules[1]).toEqual({ pattern: ['git', 'reset'], decision: 'allow', match: ['git reset --soft HEAD~1'] });
  });

  it('deletes a rule and imports the proposal only on a click', async () => {
    const ipc = createFakeRulesIpc(VIEW);
    const user = userEvent.setup();
    renderWithTheme(<RulesPanel ipc={ipc} workspaceId="w1" />);
    await screen.findByTestId('rule-workspace-1');
    expect(ipc.calls.imports).toBe(0);
    await user.click(screen.getByRole('button', { name: 'Import' }));
    expect(await screen.findByTestId('rule-workspace-2')).toHaveTextContent('make test');
    expect(ipc.calls.imports).toBe(1);
    await user.click(within(screen.getByTestId('rule-user-1')).getByRole('button', { name: 'Delete' }));
    expect(await within(screen.getByTestId('rules-scope-user')).findByText('No rules yet: the defaults apply.')).toBeInTheDocument();
  });

  it('explains which layer and rule decide a command', async () => {
    const ipc = createFakeRulesIpc(VIEW, {
      'npm publish': {
        verdict: 'deny',
        source: 'workspace',
        reason: 'Publishing goes through the release pipeline; open a PR instead.',
        rule: { scope: 'workspace', index: 1, decision: 'forbidden', pattern: 'npm publish', justification: 'Publishing goes through the release pipeline; open a PR instead.' },
        justification: 'Publishing goes through the release pipeline; open a PR instead.',
      },
    });
    const user = userEvent.setup();
    renderWithTheme(<RulesPanel ipc={ipc} workspaceId="w1" />);
    await screen.findByTestId('rule-workspace-1');
    const box = screen.getByRole('region', { name: 'Test a command' });
    await user.type(within(box).getByLabelText('Command'), 'npm publish');
    await user.click(within(box).getByRole('button', { name: 'Test' }));
    const result = await screen.findByTestId('rules-explain-result');
    expect(result).toHaveTextContent('Refused by workspace rule 1 — npm publish');
    expect(result).toHaveTextContent('open a PR instead');

    await user.clear(within(box).getByLabelText('Command'));
    await user.type(within(box).getByLabelText('Command'), 'ls');
    await user.click(within(box).getByRole('button', { name: 'Test' }));
    expect(await screen.findByText('Allowed by the defaults')).toBeInTheDocument();
    expect(ipc.calls.explained).toEqual(['npm publish', 'ls']);
  });
});
