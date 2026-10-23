/**
 * Hand-written REST client for the secrets domain (settings section).
 *
 * Source: `apps/elitea-ui/src/api/secrets.js` — RTK Query endpoints
 * (`secretsList` / `secretCreate` / `secretShow` / `secretEdit` /
 * `secretDelete` / `secretHide`).
 *
 * URL SHAPE (#151). The legacy API is
 * `/api/v2/<plugin>/<resource-module>/<mode>/<params>`. The plugin is
 * `secrets`; its resource modules are
 * `legacy/plugins/secrets/api/v2/{secrets,secret,hide}.py`. So the doubled
 * segment is real, and the baseline client agrees
 * (`apps/elitea-ui/src/api/secrets.js:3` → `apiSlicePath = '/secrets'`):
 *
 *  - GET  `/secrets/secrets/default/{projectID}`         → list (names only)
 *  - POST `/secrets/secrets/default/{projectID}`         → create
 *  - GET  `/secrets/secret/default/{projectID}/{name}`   → show (plaintext)
 *  - PUT  `/secrets/secret/default/{projectID}/{name}`   → rename / update
 *  - DELETE `/secrets/secret/default/{projectID}/{name}` → delete
 *  - POST `/secrets/hide/default/{projectID}/{name}`     → hide (move to
 *    `hidden_secrets`)
 *
 * An earlier revision of this file invented `/secrets/{mode}/…` with mode
 * `prompt_lib`; #137 then read the resulting 404s as a Go double-mount bug
 * and moved the server to match, breaking elitea-sdk, admin_ui and
 * qa/elitea-api-testing, which had always used the shape above. #151
 * restored the server and corrected this client. `secretApi.contract.test.ts`
 * pins these exact strings so the next drift fails a unit test, not a
 * cross-repo integration.
 *
 * WHY HAND-WRITTEN, NOT GENERATED: these paths are now IN
 * `services/elitea-main/api/openapi/v2.yaml` (#151 added them), so orval
 * does generate a `secrets` tag — but this slice keeps its hand-written
 * client for its bespoke envelope unwrapping (`fetchJson`) and
 * `normaliseSecrets` shaping, the same handwritten-manifest arrangement as
 * `entities/conversation/api/contextManagementApi.ts`. Migrating onto the
 * generated hooks is deliberately left out of #151's scope.
 *
 * Per R-A5, every endpoint below is reported for merge into
 * `src/shared/api/endpoints.manifest.json` as `source: "handwritten"`.
 */
import { useMutation, useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from '@tanstack/react-query';

import { eliteaFetch } from '@/shared/api/generated/mutator';

import type { Secret, RevealedSecret } from '../model/types';
import { normaliseSecrets } from './normalise';

/* ── transport helpers ─────────────────────────────────────────────────── */

/** Unwrap the `{ data: <T> }` envelope the Go handler returns. */
async function fetchJson<T>(path: string, init?: RequestInit): Promise<T> {
  const envelope = await eliteaFetch<{ data: T }>(path, init);
  return envelope.data;
}

/**
 * The legacy PLUGIN prefix for this domain. Every path below is
 * `/secrets/<resource-module>/<mode>/<params>` — see the module header.
 */
const PLUGIN_PREFIX = '/secrets';

/**
 * The legacy `c.DEFAULT_MODE` — the project-scoped handler
 * (`legacy/plugins/shared/tools/config.py:41`). The only other mode the
 * plugin defines is `administration`, which addresses the global vault and
 * is admin_ui's, not this app's. `prompt_lib` — which this client used
 * until #151 — is not a mode of this plugin at all; elitea-main now 404s it
 * rather than accepting a third convention.
 */
const SETTINGS_MODE = 'default';

function secretsBasePath(projectId: string | number): string {
  return `${PLUGIN_PREFIX}/secrets/${SETTINGS_MODE}/${projectId}`;
}

function secretPath(projectId: string | number, name: string): string {
  return `${PLUGIN_PREFIX}/secret/${SETTINGS_MODE}/${projectId}/${encodeURIComponent(name)}`;
}

function hidePath(projectId: string | number, name: string): string {
  return `${PLUGIN_PREFIX}/hide/${SETTINGS_MODE}/${projectId}/${encodeURIComponent(name)}`;
}

/* ── query key ─────────────────────────────────────────────────────────── */

function secretsQueryKey(projectId: string): string[] {
  return ['settings', 'secrets', projectId];
}

/* ── secretsList — GET /secrets/{mode}/{projectID} ─────────────────────── */
/* manifest: secrets.list */

export function listSecrets(projectId: string | number): Promise<Secret[]> {
  return fetchJson<readonly unknown[]>(secretsBasePath(projectId))
    .then((wire) => normaliseSecrets(wire));
}

export function useListSecretsQuery(projectId: string, options: { enabled?: boolean } = {}): UseQueryResult<Secret[], Error> {
  return useQuery({
    queryKey: secretsQueryKey(projectId),
    queryFn: () => listSecrets(projectId),
    enabled: options.enabled ?? true,
    refetchOnMount: true,
    refetchOnWindowFocus: false,
  });
}

/* ── secretCreate — POST /secrets/{mode}/{projectID} ───────────────────── */
/* manifest: secrets.create */

export interface CreateSecretParams {
  readonly name: string;
  readonly value: string;
}

export async function createSecret(projectId: string | number, params: CreateSecretParams): Promise<void> {
  await eliteaFetch<unknown>(secretsBasePath(projectId), {
    method: 'POST',
    body: JSON.stringify({ name: params.name, value: params.value }),
    headers: { 'Content-Type': 'application/json' },
  });
}

export function useCreateSecretMutation(projectId: string): UseMutationResult<void, Error, CreateSecretParams> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (params) => createSecret(projectId, params),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: secretsQueryKey(projectId) }),
  });
}

/* ── secretShow — GET /secret/{mode}/{projectID}/{name} ────────────────── */
/* manifest: secrets.show */

export async function showSecret(projectId: string | number, name: string): Promise<RevealedSecret> {
  return fetchJson<RevealedSecret>(secretPath(projectId, name));
}

/* ── secretEdit — PUT /secret/{mode}/{projectID}/{name} ────────────────── */
/* manifest: secrets.update */

export interface UpdateSecretParams {
  readonly name: string;
  readonly value: string;
}

export async function updateSecret(projectId: string | number, name: string, params: UpdateSecretParams): Promise<void> {
  await eliteaFetch<unknown>(secretPath(projectId, name), {
    method: 'PUT',
    body: JSON.stringify({ name: params.name, value: params.value }),
    headers: { 'Content-Type': 'application/json' },
  });
}

export function useUpdateSecretMutation(projectId: string): UseMutationResult<void, Error, { name: string; params: UpdateSecretParams }> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ name, params }) => updateSecret(projectId, name, params),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: secretsQueryKey(projectId) }),
  });
}

/* ── secretDelete — DELETE /secret/{mode}/{projectID}/{name} ───────────── */
/* manifest: secrets.delete */

export async function deleteSecret(projectId: string | number, name: string): Promise<void> {
  await eliteaFetch<unknown>(secretPath(projectId, name), { method: 'DELETE' });
}

export function useDeleteSecretMutation(projectId: string): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (name) => deleteSecret(projectId, name),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: secretsQueryKey(projectId) }),
  });
}

/* ── secretHide — POST /hide/{mode}/{projectID}/{name} ─────────────────── */
/* manifest: secrets.hide */

export async function hideSecret(projectId: string | number, name: string): Promise<void> {
  await eliteaFetch<unknown>(hidePath(projectId, name), { method: 'POST' });
}

export function useHideSecretMutation(projectId: string): UseMutationResult<void, Error, string> {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (name) => hideSecret(projectId, name),
    onSuccess: () =>
      void queryClient.invalidateQueries({ queryKey: secretsQueryKey(projectId) }),
  });
}
