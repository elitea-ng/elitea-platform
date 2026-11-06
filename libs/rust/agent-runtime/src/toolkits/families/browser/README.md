# browser family (#1596, ADR-0049)

The browser tool contract of `libs/rust/browser-tools` (`TOOLS`) on a cloud
browser session. The names, descriptions and schemas come from the contract;
do not write them here. `tests.rs` checks the toolset against
`conformance::check_exposed`.

| File | What it does |
| --- | --- |
| `mod.rs` | The toolset, one call's path (unattended rule, budget, meter, session, result shape), the run's gate |
| `category.rs` | The pure classifier: `read`, `write`, `external_send`, `payment` |
| `budget.rs` | The per-run cap and the project's monthly quota, in engine seconds |
| `session.rs` | The `BrowserSession` trait the host implements (the worker: gRPC and mTLS to the supervisor) |
| `catalogue.rs` | The `browser` entry of Main's `rust_only_toolkit_tools.json` |

## Changing the contract

Change `libs/rust/browser-tools/src/spec.rs`. Then:

1. Regenerate `rust_only_toolkit_tools.json` with
   `ELITEA_WRITE_RUST_ONLY_TOOLS=1` (the `toolkit-office` catalogue test).
2. Put each new tool in `libs/proto/contracts/tool-categories-v1.json` and
   in Main's copy (`internal/domain/toolcategory/`). Read, navigate and
   question tools are `read`; act tools are `write`. The classifier raises
   an act to `external_send` or `payment` for each call.
3. If the new tool acts on an element, add it to `category::target`.

## Changing the word lists

`PAYMENT_WORDS`, `CONSEQUENTIAL_WORDS` and `SUBMIT_WORDS` match whole words
of a control's name. A false match costs a question; a missed one is an
action nobody approved. Add a word when in doubt, and add a test line for it.

## Rules the family keeps

- A `browser_request_user` call in an unattended run fails at once with
  `no_person`. The session is not called and no time is admitted.
- A grant's `browser_submits` answers a submit on its hosts only, never a
  consequential or a payment control.
- An `egress_refused` result goes to the model unchanged and is counted in
  the run's egress ledger.
- Every message in a result is fixed text. A transport error never reaches
  the model.
- A toolset built without a host answers `browser_unavailable` for every
  call.
