import { CLIENT_VERSION_HEADER, getClientVersion } from '@/api/clientVersion';
import { ApiError } from '@/api/errors';
import { classifySessionFailure, sessionBearerToken } from '@/api/middleware';
import { apiBaseUrl, isSameOrigin } from '@/api/paths';
import { expoStreamTransport, type StreamTransport } from '@/api/sse/transport';
import { draftRequestBody, draftStreamReader, type DraftAction, type DraftRequestInput } from '@elitea/client-core/agent-builder';
import { EventStreamParser } from '@elitea/client-core/stream';
import { getSession } from '@/state/session';

/**
 * One streamed agent draft (ADR-0037, client contract 1.28):
 * `POST /elitea_core/generate_application_draft_stream/prompt_lib/{project_id}`.
 *
 * Not a `ResumableStream`: the draft stream has no event ids and is not
 * resumed. A dropped connection is a failed draft (`ended` folds it into
 * the shared cut-off failure), and a new Generate asks again. The request
 * carries what every stream carries: Bearer (refreshed first when close to
 * expiry), `X-Client-Version`, and no redirects (the transport refuses them).
 *
 * Everything it learns is dispatched as a `draftReducer` action: tokens, the
 * draft, the stream's `draft.error`, or a refusal before the stream opened
 * (its JSON `error`/`code`).
 */

const MAX_ERROR_BODY_BYTES = 4096;
const FALLBACK = 'The draft could not be generated. Try again.';

async function readSmallText(body: AsyncIterable<Uint8Array> | null): Promise<string> {
  if (!body) return '';
  const decoder = new TextDecoder();
  let text = '';
  for await (const chunk of body) {
    text += decoder.decode(chunk, { stream: true });
    if (text.length > MAX_ERROR_BODY_BYTES) return '';
  }
  return text + decoder.decode();
}

function refusal(status: number, text: string): DraftAction {
  let body: { error?: unknown; code?: unknown } = {};
  try {
    const parsed: unknown = JSON.parse(text);
    if (typeof parsed === 'object' && parsed !== null) body = parsed as typeof body;
  } catch {
    // Not JSON: the status alone names it.
  }
  return {
    type: 'error',
    failure: {
      code: typeof body.code === 'string' ? body.code : `http_${status}`,
      status,
      message: typeof body.error === 'string' && body.error !== '' ? body.error : FALLBACK,
    },
  };
}

export interface StreamDraftOptions {
  projectId: number;
  input: DraftRequestInput;
  signal: AbortSignal;
  dispatch: (action: DraftAction) => void;
  transport?: StreamTransport;
}

export async function streamAgentDraft({ projectId, input, signal, dispatch, transport = expoStreamTransport }: StreamDraftOptions): Promise<void> {
  const { serverOrigin: origin, workspaceId } = getSession();
  if (origin === null) {
    dispatch({ type: 'error', failure: { code: 'session_invalid', status: 401, message: 'Sign in to a workspace first.' } });
    return;
  }
  const url = `${apiBaseUrl(origin)}/elitea_core/generate_application_draft_stream/prompt_lib/${projectId}`;
  try {
    const token = await sessionBearerToken();
    const response = await transport(url, {
      method: 'POST',
      body: JSON.stringify(draftRequestBody(input)),
      headers: {
        Accept: 'text/event-stream',
        'Content-Type': 'application/json',
        Authorization: `Bearer ${token}`,
        [CLIENT_VERSION_HEADER]: getClientVersion(),
        'Cache-Control': 'no-cache',
      },
      signal,
    });
    if (response.url && !isSameOrigin(response.url, origin)) {
      dispatch({ type: 'error', failure: { code: 'origin_mismatch', status: 0, message: FALLBACK } });
      return;
    }
    if (response.status !== 200) {
      const text = await readSmallText(response.body);
      if (response.status === 401 || response.status === 426) {
        // The same session handling as REST: sign-out, upgrade prompt, or a renewed token.
        await classifySessionFailure({ status: response.status, json: () => Promise.resolve().then(() => JSON.parse(text) as unknown) }, { token, sessionAuth: true, workspaceId });
      }
      dispatch(refusal(response.status, text));
      return;
    }
    const reader = draftStreamReader(dispatch);
    const parser = new EventStreamParser();
    if (response.body) {
      for await (const chunk of response.body) reader.feed(parser.push(chunk));
    }
    reader.feed(parser.end());
    reader.finish();
  } catch (error) {
    if (signal.aborted) return;
    const message = error instanceof ApiError && error.kind === 'session_invalid' ? 'Sign in again to create an agent.' : "Couldn't reach the workspace. Check your connection and try again.";
    dispatch({ type: 'error', failure: { code: 'network', status: 0, message } });
  }
}
