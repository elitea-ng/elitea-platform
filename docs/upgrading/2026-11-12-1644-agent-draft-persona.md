# Agents: the draft builder proposes a persona (#1644)

- **What changed.** The agent draft routes (`generate_application_draft` and its stream) may return a
  `soul`, an `AgentSoulV1` persona. Main checks it with the same rules as a version write. A persona that
  names a capability is refused with 422 and the code `soul_capability_key`. The web review form shows the
  persona, and the create body sends it as the first version's `soul`. A refinement sends the persona back
  in `current_draft.soul`.
- **Operator action.** None. The field is optional, and a draft without a persona is unchanged. No
  migration and no client-contract change.
