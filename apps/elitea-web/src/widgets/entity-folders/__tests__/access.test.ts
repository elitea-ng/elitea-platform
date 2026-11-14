import { describe, expect, it } from 'vitest';

import { entityFolderAccess } from '../lib/access';

describe('entityFolderAccess', () => {
  it('asks each list for its OWN create and update permission', () => {
    const cases = [
      ['agent', 'models.applications.applications.create', 'models.applications.application.update'],
      ['pipeline', 'models.applications.applications.create', 'models.applications.application.update'],
      ['skill', 'models.applications.skills.create', 'models.applications.skills.update'],
      ['toolkit', 'models.applications.tools.create', 'models.applications.tool.update'],
      ['mcp', 'models.applications.tools.create', 'models.applications.tool.update'],
    ] as const;
    for (const [kind, create, update] of cases) {
      expect(entityFolderAccess(kind, new Set([create]))).toEqual({ canCreate: true, canUpdate: false });
      expect(entityFolderAccess(kind, new Set([update]))).toEqual({ canCreate: false, canUpdate: true });
      expect(entityFolderAccess(kind, new Set())).toEqual({ canCreate: false, canUpdate: false });
    }
  });

  it('does not let a skills permission open the agents list folders', () => {
    expect(entityFolderAccess('agent', new Set(['models.applications.skills.create', 'models.applications.skills.update']))).toEqual({
      canCreate: false,
      canUpdate: false,
    });
  });
});
