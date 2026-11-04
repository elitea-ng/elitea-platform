-- Preflight: no sandbox job may be live before a scenario starts; the agentstate migration head.
SELECT json_build_object(
  'live_jobs', (SELECT count(*) FROM elitea_runtime.sandbox_jobs WHERE phase IN ('reserved', 'dispatched')),
  'publishing_snapshots', (SELECT count(*) FROM elitea_runtime.rust_compiled_snapshots WHERE state = 'publishing'),
  'jobs_total', (SELECT count(*) FROM elitea_runtime.sandbox_jobs),
  'dispatches_total', (SELECT count(*) FROM elitea_runtime.sandbox_dispatches));
