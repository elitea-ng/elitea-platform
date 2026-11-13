import { describe, expect, it } from 'vitest';

import * as entity from './index';

/**
 * Pins the slice's RUNTIME public surface (spec §3.3: index.ts is the only
 * file other slices may import). `export type` interfaces are erased by
 * `verbatimModuleSyntax` and never appear on the runtime namespace object,
 * so this list is deliberately the value-export subset only — see the
 * source files for the full (type + value) surface. Precedent:
 * `entities/application/index.test.ts`.
 */
const PUBLIC_SURFACE = [
  'ApplicationConfigurationLayout',
  'ApplicationSaveButton',
  'ApplicationValidator',
  'CreateApplicationTabBar',
  'applicationCreationSchema',
  'applicationEditSchema',
  'applyMcpToolStatus',
  'isAttachmentsEnabled',
  'subApplicationTools',
  'useCreateApplicationDraft',
  'useCreateApplicationInitialValues',
  'useSaveApplicationVersion',
] as const;

describe('entities/application-form public surface', () => {
  it('exports exactly the documented runtime set', () => {
    expect(Object.keys(entity).sort()).toEqual([...PUBLIC_SURFACE].sort());
  });
});
