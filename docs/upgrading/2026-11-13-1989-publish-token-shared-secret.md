# Publish validation tokens work across replicas (#1989)

The approval token that the pre-publish check hands to the publish call was
signed with a secret generated per process. With more than one `elitea-main`
replica, a check answered by one replica and a publish answered by another
refused with `validation_token_invalid` ("not issued for this version").

- **Who is affected:** deployments that run more than one `elitea-main`
  replica (the Helm chart autoscaler does).
- **What to do:** nothing if `APPLICATION_SECRET_KEY` is set (it already is on
  every deployment). The signing secret is now derived from it, so every
  replica signs and verifies the same way. Without that variable (local
  development) the secret stays random per process.
- **Behaviour:** tokens issued before the upgrade stop verifying once (they are
  asked again, within their 30 minute life). No migration.
- **Also in this change:** `GET .../export_import/...?fork=true` honours
  `follow_version_ids` (a parameter the contract already declared) and keeps
  that version instead of the newest; an id the agent does not have answers
  404. The skills per agent version cap is 50 (was 5): `max_skills` in
  `application_skills` answers 50, and the attach route and the import wizard
  enforce 50. The agents list row now carries the first version's icon on
  `meta.icon_meta`.
