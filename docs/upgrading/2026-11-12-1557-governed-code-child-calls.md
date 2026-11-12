# Governed code-sandbox toolkit calls inherit the run's binding (issue 1557)

- **What changed.**
  - **In governed mode, a code-sandbox toolkit call that an agent run makes redeems under that run's agent binding
    and frozen credential references.** Before, it was denied with `credential_reference_not_frozen`.
  - **A direct call that you start outside any run, such as Test on a toolkit, stays denied** in governed mode. There is
    no user-scoped grant for it.
- **Operator action.** None is needed. No migration is involved. Only toolkits that the agent version froze can redeem.
- **Rollback.** Deploy the previous release. Code-sandbox toolkit calls of governed runs are denied again.
