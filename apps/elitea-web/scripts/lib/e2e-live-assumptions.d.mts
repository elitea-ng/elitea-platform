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
export declare function liveRunRefusal(env: Env): string | undefined;

export interface ChatLimits {
  readonly chat_max_upload_count: number;
  readonly chat_max_upload_size_mb: number;
  readonly chat_max_file_upload_size_mb: number;
  readonly chat_max_image_upload_count: number;
  readonly chat_max_image_upload_size_mb: number;
}
export declare const CLIENT_DEFAULT_CHAT_LIMITS: ChatLimits;
export type UploadLimitPlan =
  | { readonly ok: false; readonly reason: string }
  | { readonly ok: true; readonly discriminating: false; readonly limitMb: number; readonly reason: string }
  | {
      readonly ok: true;
      readonly discriminating: true;
      readonly limitMb: number;
      readonly oversizedBytes: number;
      readonly acceptedBytes: number;
    };
export declare function uploadLimitPlan(config: unknown): UploadLimitPlan;
export declare function schemaFilledSettings(fillableKeys: readonly string[]): Record<string, unknown>;
export declare function refusedForMissingCredential(
  status: number,
  body: unknown,
  credentialKeys: readonly string[],
): boolean;
export declare const LIVE_SAFETY_EXCLUDED: readonly string[];
export declare const LIVE_ENV_DEPENDENT: readonly string[];
export declare const LIVE_ADMIN_READONLY: readonly string[];
export declare function livePathPattern(entry: string): RegExp;
export declare function liveTestIgnore(env: Env): RegExp[];
export declare function socketServerConfigured(
  env: Env,
  uiConfig: Readonly<Record<string, unknown>> | null | undefined,
): boolean;
