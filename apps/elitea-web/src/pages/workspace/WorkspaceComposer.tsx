/**
 * The workspace session's composer: the chat page's input (`NewChatInput`:
 * Enter sends, Shift+Enter breaks the line, Send turns into Stop while a
 * turn runs) with two menus of its own.
 *
 * - "@" looks up the workspace's files and folders on the host
 *   (`workspace_files`) and inserts the picked one as an `@path` token; the
 *   picked paths still in the text go to the host as the turn's `mentions`.
 * - "/" as the first word lists the local commands (`/new`, `/plan`, …);
 *   picking one clears it from the input and hands it to the page.
 *
 * Both menus are driven from the keyboard without leaving the textarea:
 * ↑/↓ move, Enter or Tab pick, Esc closes until the token changes.
 */
import type { ComponentRef, KeyboardEvent } from 'react';
import { useMemo, useRef, useState } from 'react';

import { useQuery } from '@tanstack/react-query';

import FolderOutlinedIcon from '@mui/icons-material/FolderOutlined';
import InsertDriveFileOutlinedIcon from '@mui/icons-material/InsertDriveFileOutlined';
import SendIcon from '@mui/icons-material/Send';
import Box from '@mui/material/Box';
import FormControlLabel from '@mui/material/FormControlLabel';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Switch from '@mui/material/Switch';

import { NewChatInput } from '@/features/chat-input';
import { composer, SuggestionMenu, type SuggestionItem, type WorkspaceCommandId } from '@/features/workspace';
import type { WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

type ComposerHandle = ComponentRef<typeof NewChatInput>;
type Token = ReturnType<typeof composer.activeToken>;

const FILE_LIMIT = 50;

const NO_AGENT_EDITOR = {
  activeParticipant: undefined,
  activeParticipantDetails: undefined,
  // No participant here: no agent panel, and the model selector slot is empty.
  isAgentsPage: true,
  onSelectVersion: () => undefined,
  variables: [],
  onChangeVariables: () => undefined,
};

export interface WorkspaceComposerProps {
  ipc: WorkspaceIpc;
  workspaceId: string;
  canSend: boolean;
  busy: boolean;
  planMode: boolean;
  onPlanModeChange: (planMode: boolean) => void;
  /** Resolves `true` once the turn started (the input is then cleared). */
  onSend: (prompt: string, mentions: string[]) => Promise<boolean>;
  onStop: () => void;
  onCommand: (command: WorkspaceCommandId) => void;
}

function sendButtonSx(theme: Theme) {
  return {
    width: '1.75rem',
    height: '1.75rem',
    padding: 0,
    borderRadius: theme.vars.shape.radiusPill,
    backgroundColor: theme.vars.palette.primary.main,
    color: theme.vars.palette.icon.fill.send,
    '&:hover': { backgroundColor: theme.vars.palette.primary.main },
    '&.Mui-disabled': {
      opacity: 1,
      backgroundColor: theme.vars.palette.background.button.primary.disabled,
      color: theme.vars.palette.icon.fill.send,
    },
  };
}

const sameToken = (a: Token, b: Token): boolean => a?.kind === b?.kind && a?.start === b?.start && a?.query === b?.query;

export function WorkspaceComposer(props: WorkspaceComposerProps): React.JSX.Element {
  const { ipc, workspaceId, canSend, busy, planMode, onPlanModeChange, onSend, onStop, onCommand } = props;
  const inputRef = useRef<ComposerHandle>(null);
  const [token, setToken] = useState<Token>(null);
  const [dismissed, setDismissed] = useState<Token>(null);
  const [activeIndex, setActiveIndex] = useState(0);
  // What was picked with "@", in pick order (a folder keeps its trailing "/").
  const [picked, setPicked] = useState<string[]>([]);

  const open = token !== null && !sameToken(token, dismissed) ? token : null;
  const files = useQuery({
    queryKey: ['workspace', 'files', workspaceId, open?.kind === 'file' ? open.query : ''],
    queryFn: () => ipc.files(workspaceId, open?.query ?? '', FILE_LIMIT),
    enabled: open?.kind === 'file',
    staleTime: 5_000,
    placeholderData: (previous) => previous,
  });

  const commands = useMemo(() => (open?.kind === 'command' ? composer.matchingCommands(open.query) : []), [open]);
  const items: SuggestionItem[] = useMemo(() => {
    if (open?.kind === 'command') return commands.map((c) => ({ key: c.id, label: c.name, description: c.description }));
    if (open?.kind === 'file') {
      return (files.data ?? []).map((f) => ({
        key: `${f.kind}:${f.path}`,
        label: f.kind === 'dir' ? `${f.path}/` : f.path,
        icon: f.kind === 'dir' ? <FolderOutlinedIcon fontSize="small" /> : <InsertDriveFileOutlinedIcon fontSize="small" />,
      }));
    }
    return [];
  }, [open, commands, files.data]);
  const menuOpen = open !== null && items.length > 0;
  const highlighted = Math.min(activeIndex, Math.max(items.length - 1, 0));

  const track = (value: string): void => {
    const caret = inputRef.current?.getCursorPosition() ?? value.length;
    const next = composer.activeToken(value, caret);
    if (!sameToken(next, token)) setActiveIndex(0);
    setToken(next);
  };

  const pick = (index: number): void => {
    if (open === null) return;
    const handle = inputRef.current;
    if (open.kind === 'command') {
      const command = commands[index];
      if (command === undefined) return;
      // Without moving the caret: replaceRange would focus the input again,
      // and a command (`/agent`) may move the focus elsewhere.
      const content = handle?.getInputContent() ?? '';
      handle?.setValue(content.slice(0, open.start) + content.slice(open.end));
      setToken(null);
      onCommand(command.id);
      return;
    }
    const file = files.data?.[index];
    if (file === undefined) return;
    const text = composer.mentionText(file.path, file.kind);
    handle?.replaceRange(open.start, open.end, `${text} `);
    setPicked((current) => (current.includes(text.slice(1)) ? current : [...current, text.slice(1)]));
    setToken(null);
  };

  const onKeyDown = (event: KeyboardEvent<HTMLDivElement>): void => {
    if (!menuOpen) return;
    switch (event.key) {
      case 'ArrowDown':
        event.preventDefault();
        setActiveIndex((highlighted + 1) % items.length);
        break;
      case 'ArrowUp':
        event.preventDefault();
        setActiveIndex((highlighted - 1 + items.length) % items.length);
        break;
      case 'Enter':
      case 'Tab':
        if (event.shiftKey) return;
        event.preventDefault();
        pick(highlighted);
        break;
      case 'Escape':
        event.preventDefault();
        setDismissed(open);
        break;
      default:
    }
  };

  const send = (question: string): void => {
    const mentions = composer.referencedPaths(question, picked);
    void onSend(question, mentions).then((sent) => {
      if (!sent) return;
      inputRef.current?.reset();
      setPicked([]);
      setToken(null);
    });
  };

  return (
    <Box data-testid="workspace-composer" sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      {menuOpen && (
        <SuggestionMenu
          data-testid={open.kind === 'file' ? 'workspace-file-menu' : 'workspace-command-menu'}
          title={open.kind === 'file' ? t('workspace.composer.files', 'Files in this folder') : t('workspace.composer.commands', 'Commands')}
          items={items}
          activeIndex={highlighted}
          onPick={pick}
          onClose={() => setDismissed(open)}
        />
      )}
      <NewChatInput
        ref={inputRef}
        state={{ isStreaming: busy, disabledSend: !canSend }}
        content={{
          placeholder: t('workspace.composer.placeholder', 'What should the agent do? Type @ to add a file, / for commands'),
          clearInputAfterSubmit: false,
          tooltipOfSendButton: t('workspace.send', 'Send'),
        }}
        callbacks={{ onSend: send, onStopGeneration: onStop, onNormalKeyDown: onKeyDown, onInputChange: track }}
        agentEditor={NO_AGENT_EDITOR}
        slots={{
          modelSelector: null,
          attachmentButton: (
            <FormControlLabel
              sx={{ marginLeft: 0 }}
              control={<Switch size="small" checked={planMode} onChange={(e) => onPlanModeChange(e.target.checked)} />}
              label={t('workspace.planMode', 'Plan mode (no changes)')}
            />
          ),
          sendControl: ({ disabledSend, question, onSend: submit }) => (
            <IconButton
              data-testid="workspace-send-button"
              type="button"
              size="small"
              aria-label={t('workspace.send', 'Send')}
              disabled={disabledSend || question.trim() === ''}
              onClick={submit}
              sx={sendButtonSx}
            >
              <SendIcon fontSize="small" />
            </IconButton>
          ),
        }}
      />
    </Box>
  );
}
