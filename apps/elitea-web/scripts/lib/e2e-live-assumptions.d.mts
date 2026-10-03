/** Types for `e2e-live-assumptions.mjs` — see that file for what each decision is for. */
type Env = Readonly<Record<string, string | undefined>>;

export declare const NO_PROJECT: 'none';
export declare function envValue(env: Env, name: string): string | undefined;
export declare function e2eTenancy(env: Env): {
  readonly projectId: string;
  readonly projectName: string;
  readonly publicProjectId: string | undefined;
  readonly catalogueProjectId: string | undefined;
  readonly publishAuthorProjectId: string | undefined;
};
export declare function isLiveTarget(env: Env): boolean;
export declare function includesEnvDependent(env: Env): boolean;
export declare function shouldSkipForDeployment(absent: boolean, env: Env): boolean;
export declare function refusedOnLiveTarget(env: Env): boolean;
export declare function rethrowSkipAfterCleanup(skipped: unknown, cleanup: () => Promise<unknown>): Promise<never>;
export declare function liveRunRefusal(env: Env): string | undefined;
export declare function adminPersonaScope(env: Env): 'project' | 'platform' | undefined;
export declare function liveTraceMode(env: Env): 'off' | 'retain-on-failure';

export interface ChatLimits {
  readonly chat_max_upload_count: number;
  readonly chat_max_upload_size_mb: number;
  readonly chat_max_file_upload_size_mb: number;
  readonly chat_max_image_upload_count: number;
  readonly chat_max_image_upload_size_mb: number;
}
export declare const CLIENT_DEFAULT_CHAT_LIMITS: ChatLimits;
export declare const RIG_SEEDED_CHAT_LIMITS: ChatLimits;
export declare const PLAYWRIGHT_BUFFER_LIMIT_BYTES: number;
export type UploadLimitPlan =
  | { readonly ok: false; readonly reason: string }
  | { readonly ok: true; readonly discriminating: false; readonly limitMb: number; readonly reason: string }
  | {
      readonly ok: true;
      readonly discriminating: true;
      readonly limitMb: number;
      readonly oversizedBytes: number;
      readonly oversizedViaFile: boolean;
      readonly acceptedBytes: number;
    };
export declare function uploadLimitPlan(config: unknown): UploadLimitPlan;
export declare function nonDiscriminatingOutcome(env: Env): 'skip' | 'fail';
export declare function schemaFilledSettings(fillableKeys: readonly string[]): Record<string, unknown>;
export declare function refusedForMissingCredential(
  status: number,
  body: unknown,
  credentialKeys: readonly string[],
): boolean;
export declare function toleratesCredentialOnlyCategories(env: Env): boolean;
export declare function discoveryAbsentAnswer(status: number, body: unknown): boolean;
export declare function catalogueListsModel(status: number, body: string, modelName: string): boolean;
export declare const LIVE_SAFETY_EXCLUDED: readonly string[];
export declare const LIVE_ENV_DEPENDENT: readonly string[];
export declare const LIVE_ADMIN_READONLY: readonly string[];
export declare function livePathPattern(entry: string): RegExp;
export declare function liveTestIgnore(env: Env): RegExp[];
export declare const LIVE_API_MATCH: RegExp;
export declare const LIVE_JOURNEYS_MATCH: RegExp;
export declare const LIVE_JOURNEYS_OWN_IGNORE: readonly RegExp[];
export declare const LIVE_STREAM_ALLOWLIST: readonly RegExp[];
export declare function liveAdminReadonly(env: Env): string[];
export declare function liveSelectedSpecs(specs: readonly string[], env: Env): string[];
export declare function liveSharedStateViolations(
  source: string,
): { readonly line: number; readonly marker: string; readonly reason: string }[];
export declare function socketServerConfigured(
  env: Env,
  uiConfig: Readonly<Record<string, unknown>> | null | undefined,
): boolean;
