import { describe, expect, it } from 'vitest';

import type { PrefixRule } from '@/shared/desktop/rulesIpc';

import { conditionParts, EMPTY_GRANT, grantDraftOf, withGrant } from './grantText';

const READ_ONLY_API: PrefixRule = {
  pattern: ['gh', 'api'],
  credential: 'gh',
  hosts: ['api.github.com', 'github.com'],
  conditions: { forbid_flags: ['-f', '--input'], flag_values: { '-X': ['GET'], '--method': ['GET', 'HEAD'] }, graphql: 'query' },
};

describe('grant fields', () => {
  it('round-trips a rule through the draft', () => {
    const draft = grantDraftOf(READ_ONLY_API);
    expect(draft).toEqual({
      credential: 'gh',
      hosts: 'api.github.com github.com',
      forbidFlags: '-f --input',
      requireFlags: '',
      flagValues: '-X GET\n--method GET HEAD',
      graphqlQueryOnly: true,
    });
    expect(withGrant({ pattern: ['gh', 'api'] }, draft)).toEqual(READ_ONLY_API);
  });

  it('reads words separated by spaces, commas and lines', () => {
    const rule = withGrant({ pattern: ['gh', 'pr', 'create'] }, { ...EMPTY_GRANT, credential: 'gh', hosts: 'api.github.com,\n uploads.github.com', requireFlags: '--draft', flagValues: '-X GET\n\n-X HEAD' });
    expect(rule.hosts).toEqual(['api.github.com', 'uploads.github.com']);
    expect(rule.conditions).toEqual({ require_flags: ['--draft'], flag_values: { '-X': ['GET', 'HEAD'] } });
  });

  it('drops an empty grant and never keeps the whole network beside one', () => {
    expect(withGrant({ pattern: ['cargo'], network: true }, EMPTY_GRANT)).toEqual({ pattern: ['cargo'], network: true });
    expect(withGrant({ pattern: ['gh'], network: true }, { ...EMPTY_GRANT, credential: 'gh' })).toEqual({ pattern: ['gh'], credential: 'gh' });
  });

  it('grants nothing on a rule that is not an Allow', () => {
    const draft = grantDraftOf(READ_ONLY_API);
    expect(withGrant({ pattern: ['gh', 'api'], decision: 'prompt' }, draft)).toEqual({ pattern: ['gh', 'api'], decision: 'prompt' });
    expect(withGrant({ ...READ_ONLY_API, decision: 'forbidden' }, draft)).toEqual({ pattern: ['gh', 'api'], decision: 'forbidden' });
  });

  it('lists the conditions', () => {
    expect(conditionParts(READ_ONLY_API.conditions)).toEqual({ forbid: ['-f', '--input'], require: [], values: ['-X GET', '--method GET|HEAD'], graphql: true });
    expect(conditionParts(undefined)).toEqual({ forbid: [], require: [], values: [], graphql: false });
  });
});
