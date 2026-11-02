import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { resetConfigForTests } from '@/shared/config/get-config';
import { PERMISSIONS } from '@/shared/lib/permissions';

import { canCreate, isCreatable } from './createGate';

const base = { projectId: '2', personalProjectId: '5', creatingChat: false } as const;

beforeEach(() => {
  resetConfigForTests();
  vi.stubEnv('VITE_SERVER_URL', 'https://elitea.example');
  vi.stubEnv('VITE_BASE_URI', '/app/');
  vi.stubEnv('VITE_PUBLIC_PROJECT_ID', '11');
});

afterEach(() => {
  vi.unstubAllEnvs();
  resetConfigForTests();
});

describe('canCreate', () => {
  it('needs any one of the kind\'s create permissions', () => {
    expect(canCreate('agents', { ...base, permissions: new Set([PERMISSIONS.applications.create]) })).toBe(true);
    expect(canCreate('agents', { ...base, permissions: new Set() })).toBe(false);
    expect(canCreate('mcps', { ...base, permissions: new Set([PERMISSIONS.toolkits.create]) })).toBe(true);
    expect(canCreate('chat', { ...base, permissions: new Set([PERMISSIONS.chat.folders.create]) })).toBe(true);
  });

  it('lets anyone who sees Artifacts create a bucket', () => {
    expect(canCreate('artifacts', { ...base, permissions: new Set() })).toBe(true);
  });

  it('creates nothing in the public project for a person with no personal project', () => {
    const permissions = new Set([PERMISSIONS.applications.create]);
    expect(canCreate('agents', { ...base, permissions, projectId: '11', personalProjectId: undefined })).toBe(false);
    expect(canCreate('artifacts', { ...base, permissions, projectId: '11', personalProjectId: '' })).toBe(false);
    expect(canCreate('agents', { ...base, permissions, projectId: '11' })).toBe(true);
  });

  it('holds a new chat while one is being created', () => {
    expect(canCreate('chat', { ...base, permissions: new Set([PERMISSIONS.chat.create]), creatingChat: true })).toBe(false);
  });

  it('has no rule for rows that create nothing', () => {
    expect(isCreatable('credentials')).toBe(false);
    expect(isCreatable('catalog')).toBe(false);
    expect(isCreatable('toString')).toBe(false);
    expect(canCreate('credentials', { ...base, permissions: new Set(Object.values(PERMISSIONS.toolkits)) })).toBe(false);
  });
});
