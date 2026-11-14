import { describe, expect, it } from 'vitest';

import * as entity from './index';

/** Pins the slice's runtime public surface (types are erased and not listed). */
const PUBLIC_SURFACE = ['entityIdsInFolder', 'folderOfEntity', 'neighboursForStep', 'useEntityFolderMutations', 'useEntityFolders'] as const;

describe('entities/entity-folder public surface', () => {
  it('exports exactly the documented runtime set', () => {
    expect(Object.keys(entity).sort()).toEqual([...PUBLIC_SURFACE].sort());
  });
});
