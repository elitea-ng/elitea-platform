/**
 * Public API — spec §3.3: named exports only, curated (§3.5 budget: ≤20).
 */
export type { Pipeline, PipelineSettings, PipelineTrigger, PipelineTriggerWire } from './model/types';
export { hasSchedule, isTriggerEnabled, triggerTypeLabel } from './model/selectors';
export { normalisePipelineTrigger } from './lib/normalise';
// NOTE(#899): the hand-written pipeline-trigger client that used to live here
// spoke pylon's deleted `/elitea_core/pipeline_trigger/...` route and is gone.
// The two facilities that replaced it (`/pipeline_schedules`,
// `/pipeline_triggers`) are declared in v2.yaml, so callers use the GENERATED
// client directly — `features/pipelines/api/usePipelineTriggers.ts`.
