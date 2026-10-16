-- A dispatch row says the Worker reserved an execution activation so a stop can
-- reach its supervisor. It does not say a submission ever left the Worker: the
-- row is written before dependency hydration, and a claim lost during that
-- hydration leaves a row with no submission behind it.
--
-- `submission_pending` is TRUE from registration until the Worker durably
-- marks that it is about to send the first execute submission. The default is
-- FALSE, so every row written by an earlier Worker, and every row a Worker
-- writes without opting in, reads as "a submission may have been sent". Only
-- the opt-in registration path ever writes TRUE, and the flag only moves from
-- TRUE to FALSE.
ALTER TABLE elitea_runtime.sandbox_dispatches
    ADD COLUMN submission_pending BOOLEAN NOT NULL DEFAULT FALSE;
