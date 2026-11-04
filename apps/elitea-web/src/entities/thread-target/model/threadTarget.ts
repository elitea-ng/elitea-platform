/**
 * The `workspace_thread` target of a schedule or a webhook trigger (ADR-0045
 * decision 9, client contract 1.34): each occurrence starts a cloud turn on a
 * handed-off Local work thread in place of a run of the version.
 *
 * The form edits a draft; `threadTargetBody` turns it into the fields that the
 * schedule save and the trigger create/rotate take, and `threadTargetRefusal`
 * puts Main's refusal of a save on the field it is about.
 *
 * The grant is sent only when the person changed it. With no edit, a new
 * target sends no grant (Main's default: continue at the step and output
 * limits, inside the platform's chain limits, with the goal check). A stored
 * target sends its stored grant back, because a save without a grant resets it
 * to that default.
 */
import type { ChatUnattendedRequest } from '@/shared/api/generated/model';
import { EliteaApiError } from '@/shared/api/generated/mutator';
import { t } from '@/shared/i18n';
import { isRecord, nonBlankText } from '@/shared/lib/object';

/** The most characters a target's prompt may have (v2.yaml `prompt.maxLength`). */
export const THREAD_PROMPT_MAX = 32768;

/** The grant as the form holds it: Main's default, a grant to send, or chain limits that are not valid. */
/**
 * The grant as the form holds it: Main's default, a grant to send, or chain
 * limits that are not valid. `edit` is the grant field's own state of the
 * person's last edit (opaque here): the field reads it back, so the draft is
 * the one source of truth and the field holds no state of its own.
 */
export type ThreadGrantDraft =
  | { readonly kind: 'default' }
  | { readonly kind: 'set'; readonly grant: ChatUnattendedRequest; readonly edit?: unknown }
  | { readonly kind: 'invalid'; readonly edit: unknown };

export interface ThreadTargetDraft {
  readonly conversationUuid: string;
  readonly prompt: string;
  readonly grant: ThreadGrantDraft;
}

/** The target fields of a stored schedule or trigger read. */
export interface StoredThreadTarget {
  readonly target_kind?: string | undefined;
  readonly conversation_uuid?: string | undefined;
  readonly prompt?: string | undefined;
  readonly grant?: ChatUnattendedRequest | undefined;
}

/** The target fields of a save: `workspace_thread`, or the version's own kind to clear a stored thread. */
export interface ThreadTargetFieldsBody {
  readonly target_kind: string;
  readonly conversation_uuid?: string;
  readonly prompt?: string;
  readonly grant?: ChatUnattendedRequest;
}

/** A message per field of the thread target; `form` is a refusal that is not about one field. */
export interface ThreadTargetErrors {
  readonly thread?: string | undefined;
  readonly prompt?: string | undefined;
  /** The raw 422 `unattended_grant_refused` body; the grant field gives its text. */
  readonly grant?: unknown;
  /** The grant's chain limits are not valid; the grant field says so from the draft. */
  readonly grantInvalid?: boolean | undefined;
  readonly form?: string | undefined;
}

const WORKSPACE_THREAD = 'workspace_thread';

/** True when a stored schedule or trigger starts turns on a thread. */
export function isThreadTarget(row: StoredThreadTarget | undefined): boolean {
  return row?.target_kind === WORKSPACE_THREAD && typeof row.conversation_uuid === 'string' && row.conversation_uuid !== '';
}

/** Only the parts that a schedule or a webhook may hold: the `continue` parts, the chain limits and `goal_check`. */
function allowedGrant(grant: ChatUnattendedRequest): ChatUnattendedRequest {
  return {
    ...(grant.continue_step_limit === undefined ? {} : { continue_step_limit: grant.continue_step_limit }),
    ...(grant.continue_output_limit === undefined ? {} : { continue_output_limit: grant.continue_output_limit }),
    ...(grant.goal_check === undefined ? {} : { goal_check: grant.goal_check }),
    ...(grant.chain === undefined ? {} : { chain: grant.chain }),
  };
}

/** The draft of a stored target, or an empty one. */
export function threadTargetDraftOf(row: StoredThreadTarget | undefined): ThreadTargetDraft {
  if (!isThreadTarget(row)) return { conversationUuid: '', prompt: '', grant: { kind: 'default' } };
  return {
    conversationUuid: row?.conversation_uuid ?? '',
    prompt: row?.prompt ?? '',
    grant: row?.grant === undefined ? { kind: 'default' } : { kind: 'set', grant: allowedGrant(row.grant) },
  };
}

export type ThreadTargetBodyResult = { readonly ok: true; readonly body: ThreadTargetFieldsBody } | { readonly ok: false; readonly errors: ThreadTargetErrors };

/** The save fields of a draft, or the fields that are not complete. */
export function threadTargetBody(draft: ThreadTargetDraft): ThreadTargetBodyResult {
  const errors: ThreadTargetErrors = {
    ...(draft.conversationUuid === '' ? { thread: t('threadTarget.error.threadRequired', 'Select a thread.') } : {}),
    ...(draft.prompt.trim() === '' ? { prompt: t('threadTarget.error.promptRequired', 'Type the message that each run starts with.') } : {}),
    ...(draft.prompt.length > THREAD_PROMPT_MAX ? { prompt: t('threadTarget.error.promptTooLong', 'The message can have at most {{max}} characters.', { max: String(THREAD_PROMPT_MAX) }) } : {}),
    ...(draft.grant.kind === 'invalid' ? { grantInvalid: true } : {}),
  };
  if (Object.keys(errors).length > 0) return { ok: false, errors };
  return {
    ok: true,
    body: {
      target_kind: WORKSPACE_THREAD,
      conversation_uuid: draft.conversationUuid,
      prompt: draft.prompt,
      ...(draft.grant.kind === 'set' ? { grant: draft.grant.grant } : {}),
    },
  };
}

/** The 404 body of the thread check (`workspacethread.go`, checkThreadTarget). */
const THREAD_NOT_FOUND = /^no such conversation\b/u;

/**
 * A 404 is about the thread only when the thread check sent it: "no such
 * conversation in this project" (with no code), for a thread that is not
 * there or not the caller's. Any other 404 (the version is gone) is not.
 */
function notFoundRefusal(message: string | undefined): ThreadTargetErrors | undefined {
  if (message === undefined || !THREAD_NOT_FOUND.test(message)) return undefined;
  return { thread: t('threadTarget.refused.notFound', 'This thread is not in this project, or you are not a participant of it.') };
}

/** The refusal of a 422 or a 409, by its `code`. */
function codedRefusal(code: string | undefined, message: string | undefined): ThreadTargetErrors | undefined {
  switch (code ?? '') {
    case 'not_handed_off':
      return { thread: t('threadTarget.refused.notHandedOff', 'This thread was never sent to the cloud. Send it to the cloud from the desktop app first.') };
    case 'thread_not_governable':
      return {
        thread: t(
          'threadTarget.refused.notGovernable',
          'Credential governance is on, so a governed schedule or webhook needs a thread whose agent is this version. Select a thread that talks to this agent at this version.',
        ),
      };
    case 'thread_busy':
      return { form: t('threadTarget.refused.busy', 'The thread is busy in the cloud. Try again when the current turn ends.') };
    default:
      return message === undefined ? undefined : { form: message };
  }
}

/**
 * The refusal of a thread target save, by field, or undefined when `error` is
 * not one (the caller then reports it as any failed write).
 */
export function threadTargetRefusal(error: unknown): ThreadTargetErrors | undefined {
  if (!(error instanceof EliteaApiError) || error.failure.kind !== 'http') return undefined;
  const { status, body } = error.failure;
  const record = isRecord(body) ? body : {};
  const message = nonBlankText(record['error']);
  if (message === 'unattended_grant_refused') return { grant: body };
  switch (status) {
    case 404:
      return notFoundRefusal(message);
    case 409:
    case 422:
      return codedRefusal(nonBlankText(record['code']), message);
    case 429:
      return { form: t('threadTarget.refused.tooMany', 'This thread started too many turns. Wait a few minutes and try again.') };
    case 400:
    case 503:
      return message === undefined ? undefined : { form: message };
    default:
      return undefined;
  }
}
