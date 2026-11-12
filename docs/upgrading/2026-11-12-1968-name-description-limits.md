# Name and description length limits (issue 1968)

- **What changed.**
  - **Limits.** The name of an agent, pipeline, toolkit, credential (configuration), personal access token, folder,
    conversation or skill is at most **128 characters**, and a description (agents, pipelines, toolkits, skills) at most
    **2000**. Characters are counted as Unicode characters (an emoji is one), not bytes. Chat message bodies, agent
    instructions, welcome messages and other free text are not limited by this.
  - **A longer value is refused with 400.** The body keeps the usual envelope and names the field:
    `{"error": "name must be at most 128 characters", "field": "name", "max_length": 128}`. This replaces the
    256-character conversation name limit and the 768-byte token name limit. The contract (client contract 1.51)
    declares `maxLength` on the create and update bodies of agents, toolkits, skills and tokens; for conversations it
    is described in the schema only, because the client contract subset is additive-only.
  - **Existing data is truncated.** Tenant migration `0172_entity_name_description_limits.sql` cuts stored descriptions of
    agents, skills and toolkits to 2000 characters, folder and conversation names and credential labels to 128, and adds
    CHECK constraints on those descriptions and on folder names. A credential's `elitea_title` is the key toolkits and
    agents reference it by, so stored titles are NOT cut; only a new credential's title is held to 128. Imports and
    forks cut over-long names and descriptions instead of failing, and a write that still hits a length CHECK answers
    400, not 500. Shared migration `0242_token_name_limit.sql` cuts personal access token names to 128. The cut text is not
    kept.
  - **Clients.** The web, desktop and mobile forms show a character counter near the limit, never cut what is typed
    or pasted, and block Save past the limit with the number of characters over.
- **Who is affected.** Scripts and integrations that send a name over 128 or a description over 2000 characters now get
  400, and text stored above the limits is shortened when the migrations run. Take a backup first if you need the long
  text. Run the migrations (`elitea-migrate`, shared and tenant).
