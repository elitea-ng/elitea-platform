import { describe, expect, it } from 'vitest';

import * as entity from './index';

/**
 * Pins the slice's RUNTIME public surface (spec §3.3: index.ts is the only
 * file other slices may import). `export type` interfaces are erased by
 * `verbatimModuleSyntax` and never appear on the runtime namespace object,
 * so this list is deliberately the value-export subset only — see
 * `model/types.ts`/`lib/download.ts` for the full (type + value) surface.
 * Precedent: src/entities/bucket/index.test.ts.
 */
const PUBLIC_SURFACE = [
  'DEFAULT_ATTACHMENT_EXTENSIONS',
  'deriveAllowedAttachmentTypes',
  'fileExtension',
  'normaliseExtension',
  'useAllowedAttachmentTypes',
  'downloadAttachmentFromArtifact',
  'downloadAttachmentImage',
  'getAttachmentDisabledStatus',
  'getAttachmentName',
  'getImageSource',
  'hasUnresolvedFilepath',
] as const;

describe('entities/attachment public surface', () => {
  it('exports exactly the documented runtime set', () => {
    expect(Object.keys(entity).sort()).toEqual([...PUBLIC_SURFACE].sort());
  });
});
