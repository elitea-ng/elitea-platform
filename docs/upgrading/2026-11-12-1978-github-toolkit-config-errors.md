# Agents: a GitHub toolkit with a bad setting names it instead of failing the turn (#1978)

- **What changed.** A GitHub toolkit saved with no repository (the form's default), a malformed
  repository, a base URL that is not `https://`, or an incomplete credential used to end the whole
  turn as `INTERNAL` ("The runtime operation failed."). The turn now runs without that toolkit and
  its first message names the toolkit and the setting to edit. A blank credential base URL now means
  `https://api.github.com`.
- **Operator action.** None.
