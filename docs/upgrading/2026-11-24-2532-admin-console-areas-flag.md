# Admin console: navigation in six labelled areas, off on upgrade (#2532, ADR-0059 R3.2)

**What changed.** The admin console's sidebar can list its pages in six
labelled areas (People and access, Projects, Models and spend, Agents and
governance, Security, Platform), drawn from the console capability document
`GET /api/v2/admin/console`. Each person sees only the items their
administration roles open. Tasks and Schedules & Tasks become one item,
Schedules and tasks; both addresses keep working. A typed address to an item
the person's roles do not open shows an access-denied page naming the roles
that open it and the IT support contact, never an administrator. Roles is left
out of the areas until its page is rebuilt (R3.6); `/admin/app/roles` keeps
working.

The six areas are a platform setting, `admin_console.admin_console_areas`
(**Admin › Features › Administration console › Six labelled areas**). The
capability document carries it as `admin_console_areas`.

- A new deployment has it on (the code default).
- Shared migration `0271_admin_console_areas_default.sql` writes it **off**
  when the database already has an assignment to an administration-mode
  role, so an upgraded deployment keeps the two unlabelled groups for one
  release. It never overwrites a stored value, and it does nothing on a
  database without the role tables.

**Who is affected.** Every administrator of an upgraded deployment: nothing
changes until the setting is turned on. On a new deployment, administrators
see the six areas from the start.

**What to do.** Tell your administrators about the new navigation, then turn
on **Six labelled areas** under **Admin › Features**. A later release (#2623)
removes the setting and the two-group navigation.

**Rollback.** The previous release ignores the setting and shows the two
groups. The stored row is harmless; delete it with
`DELETE FROM centry.platform_config WHERE section = 'admin_console';` if you
want a clean table.
