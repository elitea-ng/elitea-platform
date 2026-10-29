/** yaml.rs::RawParallelNodeDefinition. Required choices remain explicit. On Error and Join take the v1.1 defaults. */
export const FixedParallelNodeDefaults: Readonly<Record<string, unknown>> = {
  branches: [],
  max_concurrency: 1,
  wait: 'all',
  on_error: 'continue',
  join: 'append',
  output: [],
  transition: 'END',
};
