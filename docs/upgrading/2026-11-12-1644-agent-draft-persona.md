# Agents: the draft builder proposes a persona (#1644)

- **What changed.** The agent draft routes (`generate_application_draft` and its stream) may return a
  `soul`, an `AgentSoulV1` persona. Main checks it with the same rules as a version write. A persona the
  model proposes that fails those rules is dropped, and the rest of the draft is returned. A persona the
  caller sends back in `current_draft.soul` that fails them is refused with 422 and the soul's own code
  (for example `soul_capability_key`). `soul_declined: true` on the stream request tells the model to
  write no persona. The web review form shows the
  persona, and the create body sends it as the first version's `soul`. A refinement sends the persona back
  in `current_draft.soul`.
- **Operator action.** None. The field is optional, and a draft without a persona is unchanged. No
  migration and no client-contract change.
