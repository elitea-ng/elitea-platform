/**
 * #937 — the two pure halves of `useToolkitCredentialIssue`. The hook itself
 * is proven end to end by `toolkits.p13-credential-warnings.spec.ts`, which
 * drives the real Agent Tools panel against a really-deleted credential.
 */
import { describe, expect, it } from 'vitest';

import { credentialLookupProjectId, selectPersonalProjectId } from './useToolkitCredentialIssue';

const PROJECT_REF = { eliteaTitle: 'shared-gh', credentialType: 'github', isPrivate: false } as const;
const PRIVATE_REF = { eliteaTitle: 'my-gh', credentialType: 'github', isPrivate: true } as const;

describe('selectPersonalProjectId', () => {
  it('reads the id off the router context, and survives every shape that carries none', () => {
    expect(selectPersonalProjectId({ auth: { getUser: () => ({ personal_project_id: '7' }) } })).toBe('7');
    expect(selectPersonalProjectId({ auth: { getUser: () => undefined } })).toBeUndefined();
    expect(selectPersonalProjectId({ auth: {} })).toBeUndefined();
    expect(selectPersonalProjectId(null)).toBeUndefined();
  });
});

describe('credentialLookupProjectId', () => {
  it('asks the toolkit\'s own project for a project credential', () => {
    expect(credentialLookupProjectId(PROJECT_REF, 'proj-1', '7')).toBe('proj-1');
  });

  it('asks the PERSONAL project for a private one — asking the wrong project would report every private credential missing', () => {
    expect(credentialLookupProjectId(PRIVATE_REF, 'proj-1', '7')).toBe('7');
  });

  it('asks nothing when the personal project is unknown, or when there is no reference', () => {
    expect(credentialLookupProjectId(PRIVATE_REF, 'proj-1', undefined)).toBeUndefined();
    expect(credentialLookupProjectId(null, 'proj-1', '7')).toBeUndefined();
  });
});
