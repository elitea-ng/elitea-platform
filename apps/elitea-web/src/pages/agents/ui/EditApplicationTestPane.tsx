/**
 * The agent page's right half: the test chat, resolved from the page's own
 * loaded detail/version (#1974, #1980).
 *
 * Its own file for two reasons: `EditApplication.tsx` sits at its §3.5 line
 * budget, and the identity narrowing below would cost it branches it does not
 * have. `EditApplicationSplit` is the layout: two columns from `md` up, the
 * pane BELOW the form on a narrow screen, where it stays reachable without
 * stealing the form's width.
 */
import { useMemo } from 'react';
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';

import type { ApplicationDetail, ApplicationVersionDetail, SocialAuthorProfile } from '@/shared/api/generated/model';
import { useGetCurrentAuthor } from '@/shared/api/generated/social/social';
import { unwrapBody } from '@/shared/api/unwrap';

import { applicationDetailDisplayName } from '../lib/editApplicationMappers';
import { AgentTestChat } from './AgentTestChat';

export interface EditApplicationTestPaneProps {
  readonly projectId: string | undefined;
  readonly applicationId: number | undefined;
  readonly detail: ApplicationDetail | undefined;
  readonly activeVersion: ApplicationVersionDetail | undefined;
  readonly isDirty: boolean;
}

/** `unwrapBody` is the one place envelope knowledge lives (R-A6); the cast is this caller's. */
function authorOf(data: unknown): SocialAuthorProfile | undefined {
  return unwrapBody(data) as SocialAuthorProfile | undefined;
}

function stringsOf(values: unknown): readonly string[] {
  return Array.isArray(values) ? values.filter((entry): entry is string => typeof entry === 'string') : [];
}

export function EditApplicationTestPane({ projectId, applicationId, detail, activeVersion, isDirty }: EditApplicationTestPaneProps): ReactNode {
  const author = authorOf(useGetCurrentAuthor().data);
  const authorId = author?.id;
  const authorName = author?.name;
  const authorAvatar = author?.avatar;
  const user = useMemo(
    () =>
      authorId === undefined
        ? undefined
        : { id: String(authorId), ...(authorName !== undefined ? { name: authorName } : {}), ...(authorAvatar !== undefined ? { avatar: authorAvatar } : {}) },
    [authorId, authorName, authorAvatar],
  );
  const agentName = detail ? applicationDetailDisplayName(detail) : undefined;
  const versionId = activeVersion?.id !== undefined ? String(activeVersion.id) : undefined;
  const agentType = activeVersion?.agent_type;
  const identity = useMemo(
    () => ({
      projectId,
      applicationId: applicationId === undefined ? undefined : String(applicationId),
      agentName,
      versionId,
      agentType,
      userId: authorId === undefined ? undefined : String(authorId),
    }),
    [projectId, applicationId, agentName, versionId, agentType, authorId],
  );
  const starters = useMemo(() => stringsOf(activeVersion?.conversation_starters), [activeVersion?.conversation_starters]);

  return (
    <AgentTestChat
      identity={identity}
      user={user}
      isDirty={isDirty}
      conversationStarters={starters}
    />
  );
}

const splitSx: SxProps<Theme> = {
  display: 'grid',
  gap: '1.5rem',
  gridTemplateColumns: { xs: 'minmax(0, 1fr)', md: 'minmax(0, 1fr) minmax(0, 1fr)' },
  alignItems: 'start',
};
const formColumnSx: SxProps<Theme> = { minWidth: 0 };
const paneColumnSx: SxProps<Theme> = {
  minWidth: 0,
  height: { xs: '32rem', md: 'calc(100vh - 11rem)' },
  position: { md: 'sticky' },
  top: { md: 0 },
};

/** `showPane` is false for a read-only public view, which may not attach the agent to a conversation: the form keeps the full width. */
export function EditApplicationSplit({ pane, showPane, children }: { readonly pane: ReactNode; readonly showPane: boolean; readonly children: ReactNode }): ReactNode {
  if (!showPane) return children;
  return (
    <Box
      data-testid="edit-application-split"
      sx={splitSx}
    >
      <Box sx={formColumnSx}>{children}</Box>
      <Box
        data-testid="edit-application-test-pane"
        sx={paneColumnSx}
      >
        {pane}
      </Box>
    </Box>
  );
}
