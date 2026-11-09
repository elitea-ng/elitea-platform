# Migrations: upgrading a database the legacy runtime built (#1843)

`elitea-migrate` can now upgrade a database that the legacy runtime built. Before this change it stopped at shared
0206 with `missing FROM-clause entry for table "e"` (42P01).

- **What changed.**
  - When `centry.project` already exists, the migrate step first adds any missing token and cost columns to
    `centry.audit_events`. The fixed list is `input_tokens`, `output_tokens`, `llm_cost`, `token_source` and
    `cost_source`. Each added column is logged at INFO ("added a column the legacy-era schema declares to a
    legacy-built table").
  - Tenant migration 0164 gives legacy-built projects these missing pieces:
    - `elitea_tools.owner_id`, set to the project's own id;
    - `elitea_tools.env_vars` and `elitea_tools.uuid`;
    - the tables `index_meta` and `social_likes`.
- **Who is affected.** Operators upgrading a database first created by the legacy runtime. On a database Main
  created, both steps do nothing.
- **What to do.**
  - Nothing beyond the usual migrate Job, which runs `elitea-migrate -all-tenants`.
  - If you added these five audit columns by hand as a workaround, they are kept as they are.
  - On a legacy-built project, the toolkit index pages show no history until a toolkit is re-indexed. Legacy index
    metadata was not stored in a table.
