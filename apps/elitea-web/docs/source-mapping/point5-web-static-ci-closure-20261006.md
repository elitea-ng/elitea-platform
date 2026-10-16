# Point5 Web static CI closure

## Source boundary

The reviewed source baseline is `1598367cee55f1ff7d3b8092caebd17becd95627`.
PR1084 static CI reports two failures after the point5 caller composition.
The budget gate measures `ApplicationAnswer.tsx` above its existing 400-line limit.
The dead-code gate identifies the unused `StateVariableConfig` export in `StateVariableList.tsx`.

## Behavioral mapping

| Existing source | Correction | Preserved behavior |
| --- | --- | --- |
| `ApplicationAnswer.tsx` loading indicator | Move its existing JSX into `ApplicationAnswerLoading.tsx`. | Retain the text, styles, animation, and streaming flag. Keep all visibility and recovery guards in the caller. |
| `StateVariableList.tsx` exported `StateVariableConfig` | Keep the interface local to its existing consumer. | Retain the same state property types and list behavior. |

The loading component is a cohesive presentation unit.
The correction does not compress source lines or change the budget.
It does not change recovery receipt validation, message identity, or pause controls.
The state drawer continues to use the raw descriptor projection from the earlier composition.
No external caller uses the removed type export.
The internal interface remains unchanged.

## Verification and limits

The actual `node scripts/check-budgets.mjs` invocation exits zero and scans 5,442 source files.
The actual `node scripts/check-dead-code.mjs` invocation exits zero.
It measures 5,544 project files and 966 entry files before running the existing strict knip gate.
Its existing `.mdx` configuration hint remains in the receipt.

Five affected unit files pass 64 tests with no skips and a direct zero exit.
They cover ApplicationAnswer, the recovery notice, ChatMessageList, StateVariableList, and StateDrawer.
Full Web typechecking and full strict lint also have direct zero exits.
The unit run uses at most two workers.
The lint run uses two threads.
The correction changes no tests, thresholds, gate configuration, or dependencies.

Local checks use Node v24.19.0 and npm 11.17.0.
Node is below the package requirement of Node >=26.
These checks do not prove the exact CI runtime, browser behavior, deployment, or runtime recovery.
No full Web unit suite or production build runs for this correction.
Existing jsdom diagnostics remain in the unit log.

The private packet retains reviewed preimages, final source hashes, and direct check logs.
Its path is `/private/tmp/elitea-graph-point5-web-static-ci-20261006/`.
The earlier frozen point5 packets remain unchanged.
