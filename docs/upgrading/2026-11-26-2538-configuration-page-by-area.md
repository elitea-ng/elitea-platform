# Admin console: Configuration sections by area, with search (#2538, ADR-0059 R3.7)

**What changed.** Admin › Configuration groups its sections under their console
area (the `access.area` each section already declares) and adds a search box
that filters the list by section title and field label. A section the caller
may read but not save opens view-only, with its fields and Save disabled.

`GET /api/v2/admin/plugin_config_schemas/administration` now adds `level`
(`read` or `write`) to each listed section: `write` exactly when a save of the
section would pass for the caller (the legacy path, or the section's narrow
write permission). Nothing was removed from the response.

**Who is affected.** Administrators using Configuration; clients of the schema
list that reject unknown fields.

**What to do.** Nothing. A client that reads the schema list can ignore `level`.
