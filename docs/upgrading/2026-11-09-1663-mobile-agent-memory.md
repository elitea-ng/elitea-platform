# Mobile: view and delete an agent's memory (#1663, ADR-0050 decision 1b)

No operator action. No migration, no new operation.

- Client contract 1.44 (additive): `listAgentMemory`, `deleteAgentMemoryEntry`
  and `setAgentMemoryOptOut` are tagged `client` and declare the
  `X-Client-Version` gate (426 below the minimum client version), like every
  other client operation. The routes, their permissions and their answers are
  unchanged.
- The mobile app shows an agent's memory (from a conversation's participants
  or a Discover agent): the notes the caller may see, delete one, and the
  "remember things about me" switch. It asks only a workspace whose discovery
  says `client_contract` 1.44 or later; on an older one the entry points are
  hidden.
