# Brand repositories (#1711, ADR-0052 W5)

A partner can now ship ELITEA under its own brand from a brand repository that
builds a released tag with one brand folder and no patches. The template is
`docs/brand-template/`; the guide is "Build your own brand" (Development).

- **Who is affected:** partners who build their own brand; maintainers of the
  desktop packaging workflow.
- **What to do:** nothing for ELITEA's own builds. A brand repository pins a
  release from v7.6.0 on in `upstream.json` and sets the signing secrets by the
  names the guide lists.
- **Behaviour:** `desktop-package.yml` now calls two composite actions,
  `.github/actions/desktop-macos-package` and
  `.github/actions/desktop-windows-package`, that a brand repository calls too.
  The secret names, the dry run on pull requests and the packages are unchanged.
  Renaming a secret or an action input is now a breaking change for brand
  repositories: say so in an upgrade note.
