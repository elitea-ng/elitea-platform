import { describe, expect, it } from 'vitest';

import type { AdminProjectRow } from './api/adminProjectsApi';
import { classifyProjectRow } from './ProjectListEditor';

function row(overrides: Partial<AdminProjectRow>): AdminProjectRow {
  return {
    id: 1,
    name: 'Team Alpha',
    owner_id: 1,
    owner_name: 'owner',
    admin_names: [],
    status: 'active',
    suspended: false,
    create_success: true,
    is_personal: false,
    ...overrides,
  };
}

describe('classifyProjectRow (A1, ELITEA-0016)', () => {
  it('is "public" for the well-known public project id, even if it also looks personal', () => {
    expect(classifyProjectRow(row({ id: 5, is_personal: true }), '5')).toBe('public');
  });

  it('is "private" for a personal project that is not the public one', () => {
    expect(classifyProjectRow(row({ id: 9, is_personal: true }), '5')).toBe('private');
  });

  it('is "team" for everything else', () => {
    expect(classifyProjectRow(row({ id: 9, is_personal: false }), '5')).toBe('team');
  });
});
