/** Authoring stays off until the separate runtime recovery and integration gates pass. */
export const FIXED_PARALLEL_AUTHORING_ENABLED = false;

export const isFixedParallelAuthoringAllowed = (type: string): boolean => type !== 'parallel' || FIXED_PARALLEL_AUTHORING_ENABLED;

/** yaml.rs::RawParallelNodeDefinition. Required choices remain explicit. No parent input or reducer exists. */
export const FixedParallelNodeDefaults: Readonly<Record<string, unknown>> = {
  branches: [],
  max_concurrency: 1,
  wait: 'all',
  error_policy: 'fail_after_drain',
  output: [],
  transition: 'END',
};
