/**
 * `testToolkitTool` — the REST half of "Test tool" (issue tracked in the
 * platform-parity wave: WP16b landed the Go side first).
 *
 * `services/elitea-main/internal/api/v2/toolkitrun/response.go` maps ONE
 * synchronous tool run onto four HTTP outcomes (its own module doc comment
 * names them verbatim):
 *
 *   OK                  200  ok:true  with the result
 *   TOOL_ERROR          200  ok:false with the tool's own sentence
 *   UNSUPPORTED_TOOLKIT 422  ok:false naming the type this image cannot build
 *   UNKNOWN_TOOL        422  ok:false naming the tool
 *   bounded wait passed 504  ok:false with the task id, so the caller can poll
 *
 * (A fifth, `runtime_failure` 500, is the boundary's own safe-message
 * catch-all — folded into `'failure'` below alongside network/403/503/etc.,
 * none of which this route can answer under normal operation.)
 *
 * The route is `POST /test_tool/prompt_lib/{projectId}/{toolId}`
 * (`toolkits.Handler.TestTool`, `router.go`) — chosen over the sibling
 * `/test_toolkit_tool/prompt_lib/{projectId}` path (which
 * `../indexes/api/indexesApi.ts`'s `startIndexExecution` already owns for
 * `index_data`) because `TestTool` is "shadowed by nothing and reachable in
 * EVERY deployment" (`internal/application/toolkitcalltool/doc.go`), while
 * the sibling path is claimed by the index-start handler wherever a runtime
 * is composed. Naming the toolkit in the URL also means the body carries
 * only `tool_name`/`tool_params` — the settings and credentials come from
 * the saved toolkit row server-side (`response.go`'s own `Body` doc: "a
 * caller that can supply settings can supply credentials").
 */
import { readToolkitTestAuthorization } from './toolkitTestAuthorization';
import type { ToolkitTestAuthorization } from './toolkitTestAuthorization';

import { eliteaFetch } from '@/shared/api/generated/mutator';

export interface TestToolkitToolParams {
  readonly requestId?: string;
  readonly projectId: string | number | undefined;
  readonly toolkitId: string | number | undefined;
  readonly toolName: string;
  readonly authorizationReference?: string;
  readonly llmModel?: string;
  readonly llmSettings?: { readonly temperature: number; readonly max_tokens: number; readonly reasoning_effort?: string };
  readonly toolParams: Readonly<Record<string, unknown>>;
}

/**
 * One outcome per branch `WriteOutcome`/`WriteError` can write. `'ok'` and
 * `'toolError'` are both a SETTLED run (the tool reached the worker); the
 * other three are refusals the run never reached the tool for.
 */
export type TestToolkitToolOutcome =
  | { readonly kind: 'authorizationRequired'; readonly challenge: ToolkitTestAuthorization; readonly taskId: string }
  | { readonly kind: 'skipped' }
  | { readonly kind: 'ok'; readonly result: unknown; readonly truncated: boolean }
  | { readonly kind: 'toolError'; readonly message: string }
  | { readonly kind: 'unsupportedToolkit'; readonly message: string }
  | { readonly kind: 'unknownTool'; readonly message: string }
  | { readonly kind: 'timeout'; readonly taskId: string | undefined; readonly message: string }
  | { readonly kind: 'failure'; readonly message: string };

interface ResponseBodyLike {
  readonly ok?: unknown;
  readonly result?: unknown;
  readonly truncated?: unknown;
  readonly error?: unknown;
  readonly reason?: unknown;
  readonly task_id?: unknown;
  readonly authorization_required?: unknown;
}

function isResponseBodyLike(value: unknown): value is ResponseBodyLike {
  return typeof value === 'object' && value !== null;
}

function readErrorMessage(body: ResponseBodyLike | undefined, fallback: string): string {
  return body !== undefined && typeof body.error === 'string' && body.error.length > 0 ? body.error : fallback;
}

/**
 * Duck-typed rather than an `instanceof EliteaApiError` check — same
 * convention as `../indexes/lib/helpers/indexExecution.helpers.ts`'s own
 * `isEliteaApiErrorLike` (that file's doc comment explains why: it keeps
 * this module testable against a hand-built fixture without importing the
 * concrete class from `shared/api/generated/mutator`).
 */
interface EliteaApiErrorLike {
  readonly failure?: { readonly kind?: string; readonly status?: number; readonly body?: unknown };
}

function isEliteaApiErrorLike(value: unknown): value is EliteaApiErrorLike {
  return typeof value === 'object' && value !== null && 'failure' in value;
}

const GENERIC_FAILURE_MESSAGE = 'The tool could not be run. Try again in a moment.';

/** The two `422` refusals — split out of `outcomeFromRejection` to keep it under the repo's complexity budget. `undefined` means "not one of these two", so the caller falls through to the generic `'failure'` mapping. */
function outcomeFrom422(reason: string | undefined, bodyLike: ResponseBodyLike | undefined): TestToolkitToolOutcome | undefined {
  if (reason === 'unsupported_toolkit') {
    return { kind: 'unsupportedToolkit', message: readErrorMessage(bodyLike, 'This toolkit type cannot run tools on this deployment.') };
  }
  if (reason === 'unknown_tool') {
    return { kind: 'unknownTool', message: readErrorMessage(bodyLike, 'This toolkit has no tool by that name.') };
  }
  return undefined;
}

function readReason(body: ResponseBodyLike | undefined): string | undefined {
  return typeof body?.reason === 'string' ? body.reason : undefined;
}

function outcomeFromAuthorization(bodyLike: ResponseBodyLike | undefined, toolkitId: TestToolkitToolParams['toolkitId']): TestToolkitToolOutcome {
    const challenge = readToolkitTestAuthorization(bodyLike?.authorization_required, toolkitId);
    const taskId = bodyLike?.task_id;
    if (challenge && typeof taskId === 'string' && taskId.length > 0 && taskId.length <= 128) return { kind: 'authorizationRequired', challenge, taskId };
    return { kind: 'failure', message: 'The authorization challenge is invalid. Run the tool again.' };
}

/** The non-2xx half of the mapping — split out to keep `testToolkitTool` under the repo's complexity budget. */
function outcomeFromRejection(error: unknown, toolkitId: TestToolkitToolParams['toolkitId']): TestToolkitToolOutcome {
  if (!isEliteaApiErrorLike(error) || error.failure?.kind !== 'http') {
    return { kind: 'failure', message: GENERIC_FAILURE_MESSAGE };
  }
  const { status, body } = error.failure;
  const bodyLike = isResponseBodyLike(body) ? body : undefined;
  const reason = readReason(bodyLike);

  if (status === 409 && reason === 'authorization_required') {
    return outcomeFromAuthorization(bodyLike, toolkitId);
  }
  if (status === 422) {
    const outcome = outcomeFrom422(reason, bodyLike);
    if (outcome !== undefined) return outcome;
  }
  if (status === 504) {
    const taskId = bodyLike !== undefined && typeof bodyLike.task_id === 'string' ? bodyLike.task_id : undefined;
    return { kind: 'timeout', taskId, message: readErrorMessage(bodyLike, 'The tool did not finish within the bounded wait.') };
  }
  return { kind: 'failure', message: readErrorMessage(bodyLike, GENERIC_FAILURE_MESSAGE) };
}

/**
 * Runs ONE tool of ONE saved toolkit through the synchronous REST route and
 * resolves to which of the five branches above it settled on. Never rejects:
 * a transport failure (network drop, an unreachable 503 deployment) is
 * folded into `'failure'` the same as a 500, so every caller gets one
 * `switch` to write instead of a `try`/`catch` of its own.
 */
export async function testToolkitTool(params: TestToolkitToolParams): Promise<TestToolkitToolOutcome> {
  const { projectId, toolkitId, toolName, toolParams } = params;
  if (params.authorizationReference !== undefined && !/^[A-Za-z0-9_-]{43}$/.test(params.authorizationReference)) return { kind: 'failure', message: 'The saved authorization reference is invalid.' };
  try {
    const envelope = await eliteaFetch<{ data: ResponseBodyLike }>(`/elitea_core/test_tool/prompt_lib/${String(projectId)}/${String(toolkitId)}`, {
      method: 'POST',
      body: JSON.stringify({
        request_id: params.requestId ?? crypto.randomUUID(),
        tool_name: toolName,
        tool_params: toolParams,
        ...(params.llmModel !== undefined ? { llm_model: params.llmModel } : {}),
        ...(params.llmSettings !== undefined ? { llm_settings: {
          temperature: params.llmSettings.temperature,
          max_tokens: params.llmSettings.max_tokens,
          ...(params.llmSettings.reasoning_effort !== undefined ? { reasoning_effort: params.llmSettings.reasoning_effort } : {}),
        } } : {}),
        ...(params.authorizationReference !== undefined ? { mcp_authorization_reference: params.authorizationReference } : {}),
      }),
      headers: { 'Content-Type': 'application/json' },
    });
    const body = envelope.data;
    if (body.ok === true) {
      return { kind: 'ok', result: body.result, truncated: body.truncated === true };
    }
    return { kind: 'toolError', message: readErrorMessage(body, 'The tool reported an error.') };
  } catch (error) {
    return outcomeFromRejection(error, toolkitId);
  }
}
