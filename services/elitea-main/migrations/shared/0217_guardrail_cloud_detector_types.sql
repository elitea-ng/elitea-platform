-- 0217_guardrail_cloud_detector_types.sql — room for the cloud classifier
-- detector types in the guardrail verdict table (ADR-0043 decision 1, issue
-- #1500).
--
-- 0214 made gateway.guardrail_verdicts.detector_type VARCHAR(16). The
-- first-party cloud adapters add the types azure_content_safety (20
-- characters), bedrock_guardrails (18) and model_armor (11). The gateway
-- cuts a value to the column width, so a 16-character column would record
-- "azure_content_sa". This migration makes the column VARCHAR(32). Widening a
-- VARCHAR keeps every row and does not rewrite the table.
--
-- No data, no permission.

ALTER TABLE gateway.guardrail_verdicts
    ALTER COLUMN detector_type TYPE VARCHAR(32);
