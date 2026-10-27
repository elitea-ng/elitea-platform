/**
 * The forge page a cloud thread's `git_push` or `open_pull_request` produced
 * (ADR-0034 decision 10, amended 2026-10-26): a pushed branch, a GitHub pull
 * request or a GitLab merge request. Read from the tool's own JSON answer, so
 * the link does not depend on the model repeating it. Only an `https:` URL of
 * an `ok` answer is a link.
 */
import { t } from '@/shared/i18n';

/** The tools' names (`elitea-agent-runtime`'s `GIT_PUSH` and `OPEN_PULL_REQUEST`). */
export const GIT_PUSH = 'git_push';
export const OPEN_PULL_REQUEST = 'open_pull_request';

export type ForgeLink =
  | { readonly kind: 'pull_request' | 'merge_request'; readonly url: string; readonly number: number | undefined }
  | { readonly kind: 'branch'; readonly url: string; readonly branch: string };

interface Answer {
  readonly status?: unknown;
  readonly url?: unknown;
  readonly pull_request?: unknown;
  readonly forge?: unknown;
  readonly branch?: unknown;
}

function httpsUrl(raw: unknown): string | undefined {
  if (typeof raw !== 'string') return undefined;
  try {
    const url = new URL(raw);
    return url.protocol === 'https:' ? url.toString() : undefined;
  } catch {
    return undefined;
  }
}

/** The `ok` answer of one of the two tools, with its `https:` URL. */
function okAnswer(name: string | undefined, output: unknown): { readonly answer: Answer; readonly url: string } | undefined {
  if ((name !== OPEN_PULL_REQUEST && name !== GIT_PUSH) || typeof output !== 'string' || !output.trimStart().startsWith('{')) return undefined;
  let answer: Answer | null;
  try {
    answer = JSON.parse(output) as Answer | null;
  } catch {
    return undefined;
  }
  const url = answer?.status === 'ok' ? httpsUrl(answer.url) : undefined;
  return answer === null || url === undefined ? undefined : { answer, url };
}

/** The link of a `git_push` or `open_pull_request` answer, if it produced one. */
export function forgeLinkOf(name: string | undefined, output: unknown): ForgeLink | undefined {
  const ok = okAnswer(name, output);
  if (ok === undefined) return undefined;
  const { answer, url } = ok;
  if (name === GIT_PUSH) return typeof answer.branch === 'string' ? { kind: 'branch', url, branch: answer.branch } : undefined;
  return { kind: answer.forge === 'gitlab' ? 'merge_request' : 'pull_request', url, number: typeof answer.pull_request === 'number' ? answer.pull_request : undefined };
}

/** The link's words: "Open pull request #7", "Open merge request !4", "Open branch x". */
export function forgeLinkLabel(link: ForgeLink): string {
  if (link.kind === 'branch') return t('chatMessages.actionView.branch', 'Open branch {{branch}}', { branch: link.branch });
  if (link.kind === 'merge_request') {
    return link.number === undefined
      ? t('chatMessages.actionView.mergeRequest', 'Open the merge request')
      : t('chatMessages.actionView.mergeRequestNumber', 'Open merge request !{{number}}', { number: String(link.number) });
  }
  return link.number === undefined
    ? t('chatMessages.actionView.pullRequest', 'Open the pull request')
    : t('chatMessages.actionView.pullRequestNumber', 'Open pull request #{{number}}', { number: String(link.number) });
}
