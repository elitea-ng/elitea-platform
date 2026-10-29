# Contributing to Elitea

Thank you for contributing.

## Before You Start

- Read [Set up a development environment](https://elitea-ng.github.io/elitea-platform/development/overview) and the architecture rules in [AGENTS.md](AGENTS.md).
- Be respectful and follow the Code of Conduct in CODE_OF_CONDUCT.md.
- For security issues, do not open a public issue. Follow SECURITY.md.

## Pull Request Guidelines

- Open a PR with a clear problem statement and scope.
- Include tests that cover your changes.
- Update the embedded docs (`apps/elitea-docs/src/content/docs`) when behavior or configuration changes.
- Keep PRs small and reviewable when possible.

## Commit Messages

This repository uses Conventional Commits.

Examples:

- feat(auth): add token refresh validation
- fix(ui): correct footer alignment on mobile
- docs: clarify oidc setup variables

The type sets the next release's version (semantic-release):

| Commit | Release |
|---|---|
| `fix: …`, `perf: …` | patch (`x.y.Z`) |
| `feat: …` | minor (`x.Y.0`) |
| `feat!: …` or a `BREAKING CHANGE:` footer | major (`X.0.0`) |
| `docs:`, `chore:`, `ci:`, `test:`, `refactor:` | none |

## Legal

No Contributor License Agreement (CLA) and no DCO sign-off are currently required.

By submitting a contribution, you confirm that:

- You have the right to submit the contribution.
- You agree to license your contribution under the repository license.

If this policy changes, it will be announced in this file and in repository settings.
