# Administration: the Entitlement Export section is saved with a write permission (#3026)

**What changed.** The **Configuration › Entitlement Export** section
(`entitlement_export`) lists the SCIM clients that may read the entitlement
tree at `/api/v2/scim/v2/entitlements`. A new permission,
`entitlements.export.manage`, now saves the section. It is a write permission
in the Security area. Until now the save was gated by `entitlements.export`,
which is a read permission. A role that was given the export only to read it,
such as an auditor, could then have changed which tools read the same data.

- `entitlements.export` alone now reads the section and is refused its save
  (`403`).
- `entitlements.export.manage` alone reads and saves the section, and opens no
  other section.
- The legacy path, `runtime.plugins` together with `entitlements.export`, still
  reads and saves the section.

The shared migration `0273_entitlement_export_manage.sql` grants
`entitlements.export.manage` to the `super_admin` and `admin` administration
roles. No other role gets it.

**Who is affected.** No one loses access. The two administrator roles hold both
permissions after the migration. A custom role that holds `entitlements.export`
without `runtime.plugins` can now open the section to read it.

**What to do.** No operator action. To let a custom role save the section, give
it `entitlements.export.manage`.
