import assert from 'node:assert/strict';
import { describe, it } from 'node:test';

import { agentBranches, initialBranch } from './branch';
import { AGENTS_MAX_TOTAL_BYTES, isValidScope, localAgentPath, localAgentPrompt, renderAgentsMd, validateLocalAgent } from './localAgent';

describe('the local agent', () => {
  it('writes the root file or a nested one', () => {
    assert.equal(localAgentPath('root'), 'AGENTS.md');
    assert.equal(localAgentPath(' ./ '), 'AGENTS.md');
    assert.equal(localAgentPath('./apps/web/'), 'apps/web/AGENTS.md');
  });

  it('admits the scopes the schema admits', () => {
    for (const scope of ['root', 'apps/web', 'src', '.']) assert.equal(isValidScope(scope), true, scope);
    for (const scope of ['/etc', '../up', 'a/../b', 'a//b', '.github']) assert.equal(isValidScope(scope), false, scope);
  });

  it('names what is wrong, and the byte cap counts UTF-8', () => {
    assert.deepEqual(validateLocalAgent({ name: ' ', instructions: '', scope: '../x' }), ['name_missing', 'instructions_missing', 'scope_invalid']);
    const wide = '€'.repeat(Math.ceil(AGENTS_MAX_TOTAL_BYTES / 3));
    assert.deepEqual(validateLocalAgent({ name: 'n', instructions: wide, scope: 'root' }), ['too_large']);
    assert.deepEqual(validateLocalAgent({ name: 'n', instructions: 'Run the tests.', scope: 'root' }), []);
  });

  it('renders the file the prompt describes', () => {
    assert.equal(renderAgentsMd({ name: ' Web ', description: 'How to work here.', instructions: 'Run npm test.\n', scope: 'root' }), '# Web\n\nHow to work here.\n\nRun npm test.\n');
  });

  it('asks the session agent to write the file behind the usual approval, and names nested scope', () => {
    const prompt = localAgentPrompt({ request: 'review React code', scope: 'apps/web', name: 'Web reviewer' });
    assert.match(prompt, /`apps\/web\/AGENTS\.md`/u);
    assert.match(prompt, /Name: Web reviewer/u);
    assert.match(prompt, /in addition to the root AGENTS\.md/u);
    assert.doesNotMatch(localAgentPrompt({ request: 'x', scope: 'root' }), /in addition to/u);
  });
});

describe('the branch question', () => {
  it('offers the local branch only in a local folder session', () => {
    assert.deepEqual(agentBranches({ localFolder: true }), ['local', 'elitea']);
    assert.deepEqual(agentBranches({ localFolder: false }), ['elitea']);
    assert.equal(initialBranch({ localFolder: false }), 'elitea');
    assert.equal(initialBranch({ localFolder: true }), undefined);
  });
});
