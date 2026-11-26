# Security signal envelope (#2325, ADR-0058 E0.1a)

**What changed.** The `elitea.audit.v1` envelope gains the `security_signal`
source and the `signal` object (ADR-0058 decision 1), plus three context fields:
`actor.role` (`subject` or `affected`), `correlation.conversation_id` (the
conversation uuid as a string) and `target.release_id` (the agent release next
to the application id). The schema also defines the `chain.checkpoint` event
(an envelope event whose `event_type` is `chain.checkpoint`, with no `signal`
object and no `chain` object, carrying `details.partition`, `epoch`,
`head_hash`, `length`, `delivery_seq_high`, `chain_status`, `key_id` and
`signature`), and states the rule that a receiver ignores fields it does not
know. `ocsf` is allowed on a signal event when the registry's OCSF class fits;
it is optional, never required. The optional `delivery_seq` field is also
declared, with the relay expected to stamp it; the relay's stamping is E11.2b.
The change is additive; `schema` stays `elitea.audit.v1` and no migration,
contract or Helm key changes.

**Who is affected.** Receivers and exporters of the audit evidence. They now
see a source they have not met (`security_signal`) and a new optional `signal`
object; they should ignore fields they do not know.

**What to do.** A receiver that validates the envelope against a previous
schema must update its schema or quarantine `security_signal` events before
`main.securitySignals.exportEnabled` is turned on: a strict `source` enum
refuses the new source. A receiver that ignores unknown fields keeps working.
If you want to receive the new stream, set
`main.securitySignals.exportEnabled` to `true` (the switch ships in E0.8a).
