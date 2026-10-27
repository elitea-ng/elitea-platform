/**
 * The branch, pull request or merge request a cloud thread's `git_push` or
 * `open_pull_request` produced (ADR-0034 decision 10, amended 2026-10-26):
 * the tool row links to it, so the link does not depend on the model
 * repeating it in its answer. Only an `https:` URL from an `ok` result is
 * linked (`forgeLinkOf`).
 */
import type { ReactNode } from 'react';

import Link from '@mui/material/Link';

import { forgeLinkLabel, forgeLinkOf } from '@/shared/lib/forgeLink';

/** @public The forge link of a tool row, opening the forge in a new tab (the desktop hands it to the system browser). */
export function PullRequestLink({ name, output }: { readonly name: string | undefined; readonly output: string | undefined }): ReactNode {
  const link = forgeLinkOf(name, output);
  if (link === undefined) return null;
  return (
    <Link
      href={link.url}
      target="_blank"
      rel="noopener noreferrer"
      data-testid="chat-tool-action-pull-request"
      variant="bodySmall"
      onClick={(event) => event.stopPropagation()}
      sx={{ display: 'block', mt: 0.25, fontWeight: 600 }}
    >
      {forgeLinkLabel(link)}
    </Link>
  );
}
