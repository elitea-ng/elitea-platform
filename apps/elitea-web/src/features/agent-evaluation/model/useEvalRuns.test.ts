import { describe, expect, it } from 'vitest';

import { evalRunQueryKeys, isRunTerminal } from './useEvalRuns';

describe('run status vocabulary', () => {
  /*
   * The FIVE strings are the reference's own (`EVAL_RUN_STATUS`), and this
   * client renders them. A status it does not know must NOT be terminal: a
   * poller that stopped on an unrecognised word would abandon a run it had
   * simply not been taught about, and the progress bar would freeze with no
   * error anywhere.
   */
  it('treats exactly finished, errored and cancelled as terminal', () => {
    expect(isRunTerminal('finished')).toBe(true);
    expect(isRunTerminal('errored')).toBe(true);
    expect(isRunTerminal('cancelled')).toBe(true);
    expect(isRunTerminal('created')).toBe(false);
    expect(isRunTerminal('running')).toBe(false);
  });

  it('does not treat an unknown or absent status as terminal', () => {
    expect(isRunTerminal('paused')).toBe(false);
    expect(isRunTerminal(undefined)).toBe(false);
  });
});

describe('query keys', () => {
  /*
   * ONE NAMESPACE PER RESOURCE, with every list and detail key nested UNDER it.
   * That nesting is what makes a single `invalidateQueries` on the root refresh
   * both the listing and the detail after a write. Two flat namespaces is how a
   * mutation succeeds, the cache is never refreshed, and the new row does not
   * appear until a reload — a 200 that looks like a write that did nothing.
   */
  it('nests every dataset key under the dataset root', () => {
    const root = evalRunQueryKeys.datasets('1');
    expect(evalRunQueryKeys.datasetList('1', undefined).slice(0, root.length)).toEqual([...root]);
    expect(evalRunQueryKeys.dataset('1', '5').slice(0, root.length)).toEqual([...root]);
  });

  it('nests every run key under the run root', () => {
    const root = evalRunQueryKeys.runs('1');
    expect(evalRunQueryKeys.runList('1', 42).slice(0, root.length)).toEqual([...root]);
    expect(evalRunQueryKeys.run('1', '9').slice(0, root.length)).toEqual([...root]);
    expect(evalRunQueryKeys.scorecard('1', '9').slice(0, root.length)).toEqual([...root]);
  });

  /*
   * The AGENT is part of the list key. Without it, opening a second agent's
   * editor would read the first agent's cached listing and show its datasets
   * under the wrong agent's name.
   */
  it('separates one agent’s listing from another’s and from the project-wide one', () => {
    const projectWide = evalRunQueryKeys.datasetList('1', undefined);
    const agentScoped = evalRunQueryKeys.datasetList('1', 42);
    const otherAgent = evalRunQueryKeys.datasetList('1', 43);
    expect(agentScoped).not.toEqual(projectWide);
    expect(agentScoped).not.toEqual(otherAgent);
  });

  it('separates one project’s keys from another’s', () => {
    expect(evalRunQueryKeys.runList('1', 42)).not.toEqual(evalRunQueryKeys.runList('2', 42));
  });
});
