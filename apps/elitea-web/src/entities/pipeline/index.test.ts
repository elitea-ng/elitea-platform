import { describe, expect, it } from 'vitest';

import * as entity from './index';

/**
 * Pins the slice's RUNTIME public surface (spec §3.3: index.ts is the only
 * file other slices may import). `export type` interfaces are erased by
 * `verbatimModuleSyntax` and never appear on the runtime namespace object,
 * so this list is deliberately the value-export subset only — see the
 * source files for the full (type + value) surface. Precedent:
 * src/shared/brand/index.test.ts. Also gives knip a live import edge into
 * this slice ahead of its Wave-2 consumers.
 */
const PUBLIC_SURFACE = [
  'hasSchedule',
  'isTriggerEnabled',
  'normalisePipelineTrigger',
  'triggerTypeLabel',
  // REMOVED by #899: the hand-written pipeline-trigger client
  // (`pipelineTriggerQueryKey` / `putPipelineTrigger` /
  // `usePipelineTriggerQuery`) spoke pylon's deleted
  // `/elitea_core/pipeline_trigger/...` route. Its two replacements are
  // declared in v2.yaml, so callers use the GENERATED client directly.
] as const;

describe('entities/pipeline public surface', () => {
  it('exports exactly the documented runtime set', () => {
    expect(Object.keys(entity).sort()).toEqual([...PUBLIC_SURFACE].sort());
  });
});
