/**
 * One thread of a folder in the sidebar's "Local work" section: its title
 * (bold while unread), its activity glyph and its cloud badge. An earlier
 * attempt sits under its rewound thread. Its context menu opens it, opens it
 * in the web chat, copies its link and renames it in place
 * (`threadNames.RenameField`: the conversation's own rename).
 */
import { useState } from 'react';

import Box from '@mui/material/Box';

import { readThreads, threadNames } from '@/features/workspace';
import type { HandoffStatus } from '@/shared/desktop/handoffIpc';
import type { Workspace } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { activityGlyph, activityTitleBold } from '@/shared/lib/conversationActivity';
import { ConversationActivityGlyph } from '@/shared/ui/ConversationActivity';

import { conversationLink, copyText } from '../lib/conversationLink';
import type { ShellAction } from '../lib/shellActions';
import { useLocalThreadActivity } from '../model/localThreadActivity';
import { useContextMenu, type ContextAction } from '../model/useContextMenu';
import { CHILD_INDENT, ShellRow } from './rows';
import { ThreadCloudBadge } from './ThreadCloudBadge';

type WorkspaceThread = ReturnType<typeof readThreads>[number];
type Run = (action: ShellAction) => void;

/** A thread's native context menu: open it, open it in the web chat, copy its link, rename it (a bound folder's only). */
function threadActions(workspace: Workspace, thread: WorkspaceThread, run: Run, rename: (() => void) | undefined): ContextAction[] {
  const link = conversationLink(thread.id);
  return [
    { id: 'open', label: t('desktop.menu.open', 'Open'), run: () => run({ type: 'open_workspace', workspaceId: workspace.id, conversationId: thread.id }) },
    { id: 'chat', label: t('workspace.openInChat', 'Open in chat'), run: () => run({ type: 'go', to: `/chat/${thread.id}` }) },
    { id: 'link', label: t('workspace.copyLink', 'Copy Link'), enabled: link !== null, run: () => link !== null && copyText(link) },
    { id: 'rename', label: t('workspace.thread.renameMenu', 'Rename…'), enabled: rename !== undefined, run: () => rename?.() },
  ];
}

function threadLabel(thread: WorkspaceThread): string {
  const title = thread.title === '' ? t('desktop.shell.untitledThread', 'Untitled thread') : thread.title;
  return thread.earlierAttemptOf === undefined ? title : t('desktop.shell.earlierAttempt', 'Earlier attempt: {{title}}', { title });
}

export interface LocalThreadRowProps {
  readonly workspace: Workspace;
  readonly thread: WorkspaceThread;
  readonly uuid: string | undefined;
  readonly selected: boolean;
  readonly run: Run;
  readonly status: HandoffStatus | undefined;
}

export function LocalThreadRow({ workspace, thread, uuid, selected, run, status }: LocalThreadRowProps): React.JSX.Element {
  const showMenu = useContextMenu();
  const activity = useLocalThreadActivity(thread.id, uuid);
  const [renaming, setRenaming] = useState(false);
  const indent = thread.earlierAttemptOf === undefined ? CHILD_INDENT : 2 * CHILD_INDENT;
  const projectId = workspace.project_id;
  if (renaming && projectId !== null) {
    return (
      <Box sx={{ display: 'flex', paddingY: 0.25, paddingRight: 1, paddingLeft: indent + 1 }}>
        <threadNames.RenameField thread={{ workspaceId: workspace.id, projectId, conversationId: thread.id }} name={thread.title} onDone={() => setRenaming(false)} />
      </Box>
    );
  }
  return (
    <ShellRow
      testId="shell-thread"
      onContextMenu={(event) => showMenu(event, threadActions(workspace, thread, run, projectId === null ? undefined : () => setRenaming(true)))}
      indent={indent}
      label={threadLabel(thread)}
      selected={selected}
      bold={!selected && activityTitleBold(activity)}
      status={<ConversationActivityGlyph kind={activityGlyph(activity)} />}
      onClick={() => run({ type: 'open_workspace', workspaceId: workspace.id, conversationId: thread.id })}
      trailing={uuid === undefined ? undefined : <ThreadCloudBadge status={status} />}
    />
  );
}
