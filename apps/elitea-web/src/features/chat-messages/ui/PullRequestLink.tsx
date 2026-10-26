/**
 * The pull request a cloud thread opened (ADR-0034 decision 10): the
 * `open_pull_request` row links to it, so the link does not depend on the
 * model repeating it in its answer. Only an `https:` URL from an `ok` result
 * is linked.
 */
import type { ReactNode } from 'react';

import Link from '@mui/material/Link';

import { t } from '@/shared/i18n';

/** The tool's name (`elitea-agent-runtime`'s `OPEN_PULL_REQUEST`). */
const OPEN_PULL_REQUEST = 'open_pull_request';

/** @public The opened pull request of an `open_pull_request` result, if it opened one. */
export function openedPullRequest(name: string | undefined, output: string | undefined): { readonly url: string; readonly number: number | undefined } | undefined {
  if (name !== OPEN_PULL_REQUEST || output === undefined || !output.trimStart().startsWith('{')) return undefined;
  try {
    const parsed = JSON.parse(output) as { readonly status?: unknown; readonly url?: unknown; readonly pull_request?: unknown } | null;
    if (parsed?.status !== 'ok' || typeof parsed.url !== 'string') return undefined;
    const url = new URL(parsed.url);
    if (url.protocol !== 'https:') return undefined;
    return { url: url.toString(), number: typeof parsed.pull_request === 'number' ? parsed.pull_request : undefined };
  } catch {
    return undefined;
  }
}

/** @public "Open pull request #N", opening the forge in a new tab. */
export function PullRequestLink({ name, output }: { readonly name: string | undefined; readonly output: string | undefined }): ReactNode {
  const opened = openedPullRequest(name, output);
  if (opened === undefined) return null;
  return (
    <Link
      href={opened.url}
      target="_blank"
      rel="noopener noreferrer"
      data-testid="chat-tool-action-pull-request"
      variant="bodySmall"
      onClick={(event) => event.stopPropagation()}
      sx={{ display: 'block', mt: 0.25, fontWeight: 600 }}
    >
      {opened.number === undefined
        ? t('chatMessages.actionView.pullRequest', 'Open the pull request')
        : t('chatMessages.actionView.pullRequestNumber', 'Open pull request #{{number}}', { number: String(opened.number) })}
    </Link>
  );
}
