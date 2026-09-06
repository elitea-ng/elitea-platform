-- What a DeepWiki generation needs in the database, on top of what
-- elitea-migrate creates. Everything here is the reduced form of the fixtures
-- apps/elitea-web/scripts/e2e-stack.sh seeds for the DeepWiki journeys; read
-- that file for the reasoning behind each row.
--
-- It uses project 1 — the "Default Project" 001_initial.sql seeds, whose
-- tenant schema p_1 that file also creates — rather than inventing a project,
-- because a new project needs its own tenant schema, its own role overrides
-- and its own vault, and none of that is what this stack is proving.

-- ── The centry vaults ────────────────────────────────────────────────────────
-- The facade resolves the code toolkit's credential THROUGH the project vault
-- before the request reaches the provider (ADR-0022 decision 6). A project
-- with no vault rows fails the whole resolve, which on the API is a 503 on
-- every generate. The blobs are Fernet and psql cannot mint them, so these are
-- the same deterministic, non-secret test blobs e2e-stack.sh writes: a fixed
-- key over an EMPTY vault.
INSERT INTO centry.secrets_key (id, data) VALUES
    ('admin', '\x6f4b47696f36536c7071656f71617172724b32757237437873724f3074626133754c6d36753779397672383d'::bytea),
    ('project-1', '\x45424553457851564668635947526f62484230654879416849694d6b4a53596e4b436b714b7977744c69383d'::bytea)
ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data;

INSERT INTO centry.secrets_data (id, data) VALUES
    ('admin', '\x674141414141426f6d544b4141414543417751464267634943516f4c4441304f44795765516b54395f515053716d754d506d585f5855576e6d566257494b58314e4d596146712d62386d6f67337145556e76336642595252484135475a666278576974436943364e764b37616c4d4f365346505558364277714c4672714546357146314b424a384d35723667426843363361625648764c32344a6d75705a434e49546c4c53674635725357726e4f333169386b4d506f4e686a4839704444333146784374726c645f4779635f6132713356735446756562786a614b313831664152715065535f5a553034776578617a74426b7a4458427977456f306e4b367a7449625f527851654c655353327a4971594c6e435a6a494b794743786e645267694968436c776874493132424f73487059774942653755527444515a772d307671617a4538706e4c4e7a45464f4c37384d527459717454392d352d37596b6a6b4864673d3d'::bytea),
    ('project-1', '\x674141414141426f6d544b4141414543417751464267634943516f4c4441304f447738384e6179597230503157334c364279534e5257346647764e5f6778596831726f472d386d646b77547a5155666a42735a785366694a62304f35726a4b2d455a707971362d5436704c7252674a4c6851395935376753595546446955383237486a36634473756a4b2d78'::bytea)
ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data;

-- ── The wiki toolkit ─────────────────────────────────────────────────────────
-- `type = 'wikis'` is the provider's own toolkit name lowercased, which is how
-- the SPI path /tools/{toolkit}/{tool}/invoke addresses it. `code_toolkit`
-- names the configuration row below; the facade REQUIRES it and refuses a
-- generate without one.
INSERT INTO p_1.elitea_tools (id, name, type, description, owner_id, author_id, settings)
VALUES (
    9002,
    'Kind Generated Wiki',
    'wikis',
    'Seeded by deploy/kind/kind-stack.sh for the DeepWiki generation check',
    1,
    1,
    '{"repository": "acme/e2e-generated", "branch": "main",
      "llm_model": "gpt-4o-mini", "code_toolkit": 9010}'::jsonb
)
ON CONFLICT (id) DO UPDATE
    SET type = EXCLUDED.type, name = EXCLUDED.name, settings = EXCLUDED.settings;

-- The repository toolkit `code_toolkit` names. The token is a literal, not a
-- {{secret.NAME}} reference: the fixture runner never clones, so nothing ever
-- presents it to GitHub.
INSERT INTO p_1.configuration
    (id, project_id, elitea_title, type, section, data, meta, shared, status_ok, source, created_at, updated_at)
VALUES
    (9010, 1, 'kind-github', 'github', 'toolkits',
     '{"base_url":"https://api.github.com","access_token":"kind-not-a-real-token"}',
     '{}', false, true, 'user', NOW(), NOW())
ON CONFLICT (id) DO UPDATE
    SET type = EXCLUDED.type, section = EXCLUDED.section, data = EXCLUDED.data, updated_at = NOW();

-- ── The bucket the provider uploads into ─────────────────────────────────────
-- The artifacts surface refuses every object route for a bucket with no row
-- (requireBucket), so without this the upload — and the listing that proves it
-- landed — answer 404.
INSERT INTO elitea_storage.buckets (project_id, name, display_name, bucket_type)
VALUES (1, 'wiki-artifacts', 'Wiki artifacts', 'local')
ON CONFLICT (project_id, name) WHERE deleted_at IS NULL DO NOTHING;

-- ── The caller's project membership ─────────────────────────────────────────
-- internal/infra/legacyrbac/postgres.go resolves a PROJECT permission from
-- auth_core__project_user_role and nothing else: a global role grants nothing
-- inside a project. Without a membership row the dev user is not a member of
-- project 1 and every facade call answers 403 "insufficient permissions".
--
-- Shared migration 0111 now gives project 1 the roles a provisioned project
-- gets — admin, editor, viewer and system — so the INSERT below usually
-- writes nothing. It stays for the database 0111 skips: 0111 seeds the roles
-- only when centry.project already holds id 1 when it runs.
--
-- The MEMBERSHIP is what this file still owns. 0111 creates roles and makes
-- nobody a member of anything.
-- DELIBERATELY WITHOUT auth_core__project_role_permission ROWS: the resolver
-- falls back to the CENTRAL matrix for a project role that carries no
-- overrides, and the central matrix is where shared migration 0106 grants
-- models.applications.deepwiki.read and .generate. Writing overrides here
-- would replace that whole matrix with whatever this file happened to list —
-- the freeze-out 0106's own comment warns about.
INSERT INTO auth_core__project_role (project_id, name) VALUES
    (1, 'admin'), (1, 'editor'), (1, 'viewer')
ON CONFLICT (project_id, name) DO NOTHING;

INSERT INTO auth_core__project_user_role (project_id, user_id, role_id)
SELECT 1, 1, r.id
FROM auth_core__project_role r
WHERE r.project_id = 1 AND r.name = 'admin'
ON CONFLICT (project_id, user_id, role_id) DO NOTHING;

-- ── The credential `verify` authenticates with ───────────────────────────────
-- A personal access token for the dev user 001_initial.sql seeds. The row is
-- the credential; the bearer is a HS512 JWT over {"uuid": …} signed with
-- APPLICATION_SECRET_KEY, which is what authsvc.LocalValidator — the API
-- group's token validator on an OIDC-only stack — verifies and then looks up
-- here. `expires` NULL means no expiry, which the validator's query accepts.
INSERT INTO public.auth_core__token (uuid, expires, user_id, name)
SELECT '4b1d0000-0000-4000-8000-00000000d0c5', NULL, 1, 'kind-verify'
WHERE NOT EXISTS (
    SELECT 1 FROM public.auth_core__token
    WHERE uuid = '4b1d0000-0000-4000-8000-00000000d0c5'
);
