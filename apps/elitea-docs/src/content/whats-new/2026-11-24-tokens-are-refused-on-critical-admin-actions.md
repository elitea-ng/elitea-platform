---
date: 2026-11-24
---
## Tokens are refused on critical administration actions

A personal access token can no longer change who is an administrator, edit role permissions, delete a role or a project, or make the other critical administration changes. It gets `403` with the code `token_critical_action_refused`, and the refusal is recorded in the audit trail. Sign in to the console for these changes. Other administration actions keep working with the token. So that a broken sign-in stays repairable, an `inherit` token of a global administrator can still make the identity provider and SCIM trust changes. See [Tokens and critical actions](/admin/authentication#tokens-and-critical-actions).
