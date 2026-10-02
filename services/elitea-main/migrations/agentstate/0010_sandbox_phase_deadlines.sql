-- Keep readiness and execution clocks independent from reservation and lease renewal.
ALTER TABLE elitea_runtime.sandbox_jobs
    ADD COLUMN runtime_bound_at TIMESTAMPTZ,
    ADD COLUMN dispatched_at TIMESTAMPTZ;

-- Preserve the old deadline for existing jobs. Do not grant a new observation budget.
UPDATE elitea_runtime.sandbox_jobs
SET runtime_bound_at = created_at
WHERE runtime_id IS NOT NULL;

UPDATE elitea_runtime.sandbox_jobs
SET dispatched_at = created_at
WHERE phase <> 'reserved';
