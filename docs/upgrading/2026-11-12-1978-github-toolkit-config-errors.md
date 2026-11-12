# Agents: a GitHub toolkit with a bad setting names it instead of failing the turn (#1978)

- **What changed.** A GitHub toolkit saved with no repository (the form's default), a malformed
  repository, a base URL that is not `https://`, or an incomplete credential used to end the whole
  turn as `INTERNAL` ("The runtime operation failed."). The turn now runs without that toolkit and
  its first message names the toolkit and the setting to edit. Callers that cannot show that notice
  (pipelines, nested and direct toolkit calls) still fail, with `InvalidConfiguration` and the named
  reason. A blank credential base URL is still refused — it is never defaulted to
  `https://api.github.com`, so a GitHub Enterprise token is never sent to the public API.
- **Operator action.** None.
