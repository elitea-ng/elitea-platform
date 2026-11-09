# Upgrade notes

One file per change that needs an operator to act when a deployment moves to a
newer release. The docs site assembles the files into
[Deployment › Upgrading](https://elitea-ng.github.io/elitea-platform/deployment/upgrading/),
the newest first. `docs/UPGRADING.md` only points here.

Before this directory existed, every pull request added its entry to the top of
one shared file, so any two open pull requests with an entry had a merge
conflict. A new file per change cannot conflict.

## Rules

1. **One new file per change.** Add `docs/upgrading/<number>-<slug>.md`.
   - `<number>` is the issue number (or the pull request number when there is
     no issue), at least four digits: `0042`, `1833`.
   - `<slug>` is lowercase letters, digits and single hyphens, from the title:
     `1833-upgrade-notes-are-one-file-per-change.md`.
   - Two changes for one issue get two files with the same number and
     different slugs.
2. **Never edit or delete a file that another pull request added.** Your pull
   request may change the files that it adds. To correct a merged entry, add
   the label `upgrade-note-edit` to the pull request, then push again (a
   label alone does not start the check).
3. **Do not edit `docs/UPGRADING.md`.** It is a fixed pointer.
4. **The newest entry is first.** The page sorts by `date` (newest first).
   Entries of one date go by `order` when they have it (the entries moved
   from `docs/UPGRADING.md` keep their old order this way), and the entries
   without `order` come first, by `issue` (highest first), then by file name.
   Do not set `order` on a new entry.

`scripts/ci/check-upgrading-fragments.mjs` (no-binaries.yml, job "Client
feature-parity table and upgrade notes are well formed") enforces these rules
and the front matter below.

## Template

```markdown
---
title: "What changed, in a few words (#1833, ADR-0052 W3)"
issue: 1833
date: 2026-11-09
adr: ADR-0052
---

What changed, who is affected, and what to do. Write it for the operator who
upgrades a deployment.

- **Upgrade order:** say so when one component must roll out before another.
- **Migration:** say so, or say "No migration."
- **Breaking:** say what the chart or the service now refuses, and the fix.
```

- `title`: required, in double quotes. The page shows it as the entry's
  heading. Inline code in backticks is allowed. Put the issue number and the
  ADR in parentheses at the end, as the other entries do.
- `issue`: required, the same number as the file name.
- `date`: required, `YYYY-MM-DD`, the day you write the entry. Not in the
  future.
- `adr`: optional, `ADR-NNNN`.
- `order`: only on the entries moved from `docs/UPGRADING.md`.

The body is Markdown without the title. Use `###` for a sub-heading, never `#`
or `##`. A link to a docs page is a relative path to its `.mdx` file
(`../../apps/elitea-docs/src/content/docs/deployment/scaling.mdx`): it works on
GitHub, and the site turns it into a link to the page. Any other relative link
goes to the file on GitHub.

Each entry's anchor on the page is its file name without `.md`:
`/deployment/upgrading#1833-upgrade-notes-are-one-file-per-change`.
