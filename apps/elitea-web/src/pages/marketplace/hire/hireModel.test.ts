import { describe, expect, it } from 'vitest';

import type { MarketplaceRequirements, MarketplaceVersion } from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';

import { versionActions } from '../ui/versionActions';

import {
  bindingsOf,
  defaultVersionId,
  hireErrorCode,
  hireErrorMessage,
  isHireable,
  needsApproval,
  unboundRequirements,
} from './hireModel';

const NONE: MarketplaceRequirements = { items: [], model_class: 'chat', tier: 'low', has_write_tools: false };

function version(id: string, state: string, overrides: Partial<MarketplaceVersion> = {}): MarketplaceVersion {
  return {
    id,
    name: `v${id}`,
    state,
    release_notes: '',
    digest: 'abcdef0123456789',
    published_at: '2026-10-30T09:00:00Z',
    requirements: NONE,
    ...overrides,
  };
}

const GITHUB: MarketplaceRequirements = {
  items: [
    {
      key: 'toolkit:github',
      kind: 'toolkit',
      type: 'github',
      name: 'Release bot',
      tools: ['create_release'],
      has_write_tools: true,
    },
    { key: 'skill:triage', kind: 'skill', type: 'triage', name: 'triage', tools: [], has_write_tools: false },
  ],
  model_class: 'tool_calling',
  tier: 'medium',
  has_write_tools: true,
};

describe('versions', () => {
  it('hires a deprecated version with a warning, never a yanked or withdrawn one', () => {
    expect(isHireable(version('1', 'deprecated'))).toBe(true);
    expect(isHireable(version('1', 'yanked'))).toBe(false);
    expect(isHireable(version('1', 'available', { withdrawn: true }))).toBe(false);
  });

  it('starts on the current version, else the newest that can be hired', () => {
    expect(defaultVersionId([version('3', 'current'), version('2', 'available')])).toBe('3');
    expect(defaultVersionId([version('3', 'yanked'), version('2', 'available')])).toBe('2');
    expect(defaultVersionId([version('3', 'yanked')])).toBe('');
  });

  it('offers each person only the state changes they may make', () => {
    expect(versionActions(version('1', 'current'), true, false)).toEqual(['deprecated', 'yanked']);
    expect(versionActions(version('1', 'deprecated'), true, false)).toEqual(['yanked', 'available']);
    expect(versionActions(version('1', 'yanked'), true, false)).toEqual(['available']);
    expect(versionActions(version('1', 'current'), false, true)).toEqual(['yanked']);
    expect(versionActions(version('1', 'current'), false, false)).toEqual([]);
  });
});

describe('bindings', () => {
  it('cannot finish while a required toolkit is unbound, but a skill is optional', () => {
    expect(unboundRequirements(GITHUB, {}).map((r) => r.key)).toEqual(['toolkit:github']);
    expect(unboundRequirements(GITHUB, { 'toolkit:github': { kind: 'entity', id: '12' } })).toEqual([]);
  });

  it('lets a person disable a toolkit for now', () => {
    expect(unboundRequirements(GITHUB, { 'toolkit:github': { kind: 'disabled' } })).toEqual([]);
    expect(bindingsOf(GITHUB, { 'toolkit:github': { kind: 'disabled' } })).toEqual([
      { requirement_key: 'toolkit:github', disabled: true },
    ]);
  });

  it('sends one binding per choice made', () => {
    expect(
      bindingsOf(GITHUB, {
        'toolkit:github': { kind: 'entity', id: '12' },
        'skill:triage': { kind: 'entity', id: '4' },
      }),
    ).toEqual([
      { requirement_key: 'toolkit:github', toolkit_id: '12' },
      { requirement_key: 'skill:triage', toolkit_id: '4' },
    ]);
  });
});

describe('approval', () => {
  it('asks for a change request above low tier or with write tools', () => {
    expect(needsApproval(NONE)).toBe(false);
    expect(needsApproval({ ...NONE, tier: 'medium' })).toBe(true);
    expect(needsApproval({ ...NONE, has_write_tools: true })).toBe(true);
  });
});

describe('refusals', () => {
  const refusal = (code: string): EliteaApiError =>
    new EliteaApiError({ kind: 'http', status: 409, url: '/hire', body: { error: code } });

  it('reads the server code', () => {
    expect(hireErrorCode(refusal('binding_missing'))).toBe('binding_missing');
    expect(hireErrorCode(new Error('x'))).toBeUndefined();
  });

  it('tells a personal project to use a shared one', () => {
    expect(hireErrorMessage(refusal('hire_needs_shared_project'))).toMatch(/shared project/);
  });

  it('gives an unknown refusal a plain failure line', () => {
    expect(hireErrorMessage(refusal('other'))).toBe('The hire failed. Try again.');
  });
});
