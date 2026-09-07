import { describe, expect, it } from 'vitest';

import { buildParticipantCandidates } from './participantCandidates';
import type { ToolkitCandidate } from './toolkitParticipants';
import type { Application, PublicApplicationSummary, UserRecord } from '@/shared/api/generated/model';

function application(overrides: Partial<Application> = {}): Application {
  return {
    id: 'app-1',
    name: 'Zebra Agent',
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    owner_id: 'u1',
    is_forked: false,
    meta: null,
    has_interrupt: false,
    ...overrides,
  };
}

function publicApplication(overrides: Partial<PublicApplicationSummary> = {}): PublicApplicationSummary {
  return {
    project_id: '1',
    id: 'pub-1',
    name: 'Alpha Public',
    description: '',
    version_id: 'v1',
    version_name: 'v1',
    agent_type: 'classic',
    meta: null,
    tags: [],
    likes: 0,
    is_liked: false,
    ...overrides,
  };
}

function toolkit(overrides: Partial<ToolkitCandidate> = {}): ToolkitCandidate {
  return { id: 'tk-1', name: 'Github', type: 'github', ...overrides };
}

function user(overrides: Partial<UserRecord> = {}): UserRecord {
  return { id: 'u-1', email: 'u@example.com', name: 'Bob', roles: [], ...overrides };
}

describe('buildParticipantCandidates', () => {
  it('merges every source, sorts alphabetically case-insensitively, and tags participantType/isPublic', () => {
    const result = buildParticipantCandidates({
      privateApplications: [application({ name: 'Zebra Agent' })],
      publicApplications: [publicApplication({ name: 'alpha public' })],
      privatePipelines: [],
      publicPipelines: [],
      privateToolkits: [toolkit({ name: 'Middle Toolkit' })],
      publicToolkits: [],
      privateMcps: [],
      publicMcps: [],
      users: [],
      types: [],
    });

    expect(result.map((r) => r.label)).toEqual(['alpha public', 'Middle Toolkit', 'Zebra Agent']);
    expect(result[0]).toMatchObject({ participantType: 'application', isPublic: true });
    expect(result[1]).toMatchObject({ participantType: 'toolkit', isPublic: false });
    expect(result[2]).toMatchObject({ participantType: 'application', isPublic: false });
  });

  it('falls a blank application name back to "Untitled"', () => {
    const result = buildParticipantCandidates({
      privateApplications: [application({ name: '  ' })],
      publicApplications: [],
      privatePipelines: [],
      publicPipelines: [],
      privateToolkits: [],
      publicToolkits: [],
      privateMcps: [],
      publicMcps: [],
      users: [],
      types: [],
    });
    expect(result[0]?.label).toBe('Untitled');
  });

  it('tags pipelines distinctly from applications', () => {
    const result = buildParticipantCandidates({
      privateApplications: [],
      publicApplications: [],
      privatePipelines: [application({ name: 'My Pipeline' })],
      publicPipelines: [],
      privateToolkits: [],
      publicToolkits: [],
      privateMcps: [],
      publicMcps: [],
      users: [],
      types: [],
    });
    expect(result[0]).toMatchObject({ participantType: 'pipeline', isPublic: false });
  });

  it('excludes currentUserId from the user list, and falls a blank user name back to email', () => {
    const result = buildParticipantCandidates({
      privateApplications: [],
      publicApplications: [],
      privatePipelines: [],
      publicPipelines: [],
      privateToolkits: [],
      publicToolkits: [],
      privateMcps: [],
      publicMcps: [],
      users: [user({ id: 'me', name: 'Me' }), user({ id: 'other', name: '', email: 'other@example.com' })],
      currentUserId: 'me',
      types: ['user'],
    });
    expect(result).toHaveLength(1);
    expect(result[0]).toMatchObject({ label: 'other@example.com', participantType: 'user' });
  });

  it('an empty `types` array includes applications/pipelines/toolkits/users (this pure merge step has no fetch-gating asymmetry — that lives in useParticipants)', () => {
    const result = buildParticipantCandidates({
      privateApplications: [application()],
      publicApplications: [],
      privatePipelines: [],
      publicPipelines: [],
      privateToolkits: [toolkit()],
      publicToolkits: [],
      privateMcps: [],
      publicMcps: [],
      users: [user()],
      types: [],
    });
    expect(result.map((r) => r.participantType).sort()).toEqual(['application', 'toolkit', 'user']);
  });

  it('a non-empty `types` array filters strictly, mapping "pipeline" items under the "application" bucket', () => {
    const result = buildParticipantCandidates({
      privateApplications: [application({ name: 'App' })],
      publicApplications: [],
      privatePipelines: [application({ name: 'Pipeline' })],
      publicPipelines: [],
      privateToolkits: [toolkit()],
      publicToolkits: [],
      privateMcps: [],
      publicMcps: [],
      users: [user()],
      types: ['application'],
    });
    expect(result.map((r) => r.label).sort()).toEqual(['App', 'Pipeline']);
  });
});
