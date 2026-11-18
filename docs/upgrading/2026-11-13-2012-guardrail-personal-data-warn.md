# Personal data rules, the warn action and chat notices for guardrails (#2012)

The LLM gateway's guardrail engine has a built-in `personal_data` detector and a
`warn` action. Main has a new read, `GET /api/v2/elitea_core/guardrail_notices/prompt_lib/{projectID}/{runID}`
(`listRunGuardrailNotices`), and the web, desktop and mobile chat show a notice
when a message was blocked, redacted or warned about. Admin › LLM Governance has a
**Credentials / Personal data** preset.

- **Who is affected:** administrators who write guardrail entries; people who
  chat (they now see a notice); client builders (additive change, client contract
  1.54).
- **What to do:** apply the shared migrations (`elitea-migrate`) BEFORE the new
  gateway starts to write `warn` verdicts. Migration
  `0247_guardrail_warn_action.sql` widens the CHECK constraints of
  `gateway.guardrail_verdicts` (`action` gains `warn`, `outcome` gains `warned`).
  A gateway that writes a `warn` verdict to a database without it fails the
  verdict INSERT of that batch; the request log rows of the batch are kept and
  `gateway_guardrail_verdict_write_failures_total` counts the lost verdicts. Roll
  out in this order: migrations, Main, gateway, worker, then the apps. No new
  setting.
- **Behaviour:**
  - `personal_data` detector (rules `email`, `phone`, `payment-card`, `iban`; set
    `rules` for a subset). The label of a finding is the rule id. A card needs a
    known issuer prefix and a valid Luhn check; an IBAN needs a registered country,
    the registered length and a valid mod-97 check; asset names such as
    `icon@2x.png` and the `example.*` domains are not e-mail addresses.
  - `warn` action: changes nothing, records the verdict outcome `warned`. A
    row of only `flag` and `warn` detectors defaults to fail mode `open`.
    Streamed output is not held back for a warn detector.
  - The notices route takes `models.chat.messages.create`, not
    `runs.evidence.read`. Its answer has no inspected or matched text.
  - A gateway block (HTTP 400 `guardrail_blocked`) now has the worker code
    `model_gateway.guardrail_blocked` and still ends the run with
    `MODEL_REQUEST_REJECTED` (before, the worker code was the generic
    `model_gateway.rejected`, so nothing could tell a guardrail from another
    rejection). A Local work turn of the desktop app reports the same code.
- **Not changed:** a message is still stored as written. Only what goes to the
  model is masked. Inspecting a message before it is stored needs an amendment of
  ADR-0043.
