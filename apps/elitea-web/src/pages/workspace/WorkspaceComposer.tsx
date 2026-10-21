/**
 * The workspace session's composer: the chat page's input (`NewChatInput`:
 * Enter sends, Shift+Enter breaks the line, Send turns into Stop while a
 * turn runs) with two menus of its own.
 *
 * - "@" looks up the workspace's files and folders on the host
 *   (`workspace_files`) and inserts the picked one as an `@path` token; the
 *   picked paths still in the text go to the host as the turn's `mentions`.
 * - "/" as the first word lists the local commands (`/new`, `/plan`, …);
 *   picking one clears it from the input and hands it to the page. Below
 *   them, in sections of their own, the custom commands (the workspace's,
 *   then the person's; `commands_list`) and the selected agent's skills:
 *   picking one inserts `/<name> `, and a message that still starts with
 *   it is sent as that command (`command`, which the host reads and
 *   expands itself) or with that skill (`skills`, applied to the turn).
 *   `/model <name>` sent as the whole message sets the turn model instead
 *   (`onModelCommand`); `/model` alone opens the model chip's menu.
 *
 * Both menus are driven from the keyboard without leaving the textarea:
 * ↑/↓ move, Enter or Tab pick, Esc closes until the token changes.
 */
import { useMemo, useRef, useState, type ComponentRef, type KeyboardEvent, type ReactNode } from 'react';

import { useQuery } from '@tanstack/react-query';

import FolderOutlinedIcon from '@mui/icons-material/FolderOutlined';
import InsertDriveFileOutlinedIcon from '@mui/icons-material/InsertDriveFileOutlined';
import SendIcon from '@mui/icons-material/Send';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';

import { NewChatInput } from '@/features/chat-input';
import { composer, SuggestionMenu, type SuggestionItem, type WorkspaceCommandId } from '@/features/workspace';

import type { ComposerSkill } from './useAgentSkills';
import type { CustomCommand, TurnCommand, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';
import { Segmented } from '@/widgets/desktop-shell';

type ComposerHandle = ComponentRef<typeof NewChatInput>;
type Token = ReturnType<typeof composer.activeToken>;
type SlashEntry = ReturnType<typeof composer.slashMenu>[number];

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
  /** The thread runs in the cloud: the input is off and `canSend` is false (its banner says why and offers Take back). */
  readOnly?: boolean;
  busy: boolean;
  planMode: boolean;
  /** The footer's agent·version picker and run-location chip (`ComposerControls`). */
  agentPicker?: ReactNode;
  /** A thread in the cloud: the placeholder says who the message goes to. */
  placeholder?: string | undefined;
  /** Act | Plan: off for a cloud thread, whose turns have no plan mode. */
  showMode?: boolean;
  runLocation?: ReactNode;
  /** The context meter (`ContextMeter`), after the run location. */
  contextMeter?: ReactNode;
  /** The model chip (`ModelChip`), after the agent picker. */
  modelPicker?: ReactNode;
  onPlanModeChange: (planMode: boolean) => void;
  /** The selected agent's skills, offered after the commands in the "/" menu. */
  skills: readonly ComposerSkill[];
  /** The custom commands (`commands_list`), offered after the host commands; none for a cloud thread. */
  customCommands: readonly CustomCommand[];
  /**
   * Resolves `true` once the turn started (the input is then cleared). `skills`: the skill names the message invokes;
   * `command`: the custom command it runs, if any (then no skill).
   */
  onSend: (prompt: string, mentions: string[], skills: string[], command?: TurnCommand) => Promise<boolean>;
  onStop: () => void;
  onCommand: (command: WorkspaceCommandId) => void;
  /** `/model <name>` was sent as the whole message: the page sets the model (nothing goes to the agent). */
  onModelCommand?: ((name: string) => void) | undefined;
  /** The composer got focus: the page lets the host read ahead for the send (#1213). */
  onFocus?: () => void;
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

/** The input's hint: what to type, or why it is off. */
function placeholderFor(readOnly: boolean): string {
  return readOnly
    ? t('workspace.composer.cloudPlaceholder', 'This thread is running in the cloud. Take it back to continue here.')
    : t('workspace.composer.placeholder', 'What should the agent do? Type @ to add a file, / for commands and skills');
}

/** One "/" menu row: host commands under the menu's title, then a section per source, then the skills. */
function slashItem(entry: SlashEntry): SuggestionItem {
  switch (entry.kind) {
    case 'host':
      return { key: entry.command.id, label: entry.command.name, description: entry.command.description };
    case 'custom':
      return {
        key: `command:${entry.command.source}:${entry.command.name}`,
        label: composer.customCommandToken(entry.command),
        description: composer.customCommandDetail(entry.command),
        section:
          entry.command.source === 'workspace'
            ? t('workspace.composer.workspaceCommands', 'Commands in this workspace')
            : t('workspace.composer.userCommands', 'Your commands'),
      };
    case 'skill':
      return {
        key: `skill:${entry.skill.id}`,
        label: composer.skillToken(entry.skill),
        section: t('workspace.composer.skills', 'Skills'),
        ...(entry.skill.description !== undefined ? { description: entry.skill.description } : {}),
      };
  }
}

/** What picking a custom command or a skill inserts. */
function insertedToken(entry: Exclude<SlashEntry, { kind: 'host' }>): string {
  return entry.kind === 'custom' ? composer.customCommandToken(entry.command) : composer.skillToken(entry.skill);
}

const sameToken = (a: Token, b: Token): boolean => a?.kind === b?.kind && a?.start === b?.start && a?.query === b?.query;

interface FooterControlsProps {
  planMode: boolean;
  onPlanModeChange: (planMode: boolean) => void;
  showMode: boolean;
  agentPicker: ReactNode;
  modelPicker: ReactNode;
  runLocation: ReactNode;
  contextMeter: ReactNode;
}

/** The footer's left side: Act | Plan, the agent·version picker, the model, where the thread runs, the context meter. */
function FooterControls({ planMode, onPlanModeChange, showMode, agentPicker, modelPicker, runLocation, contextMeter }: FooterControlsProps): React.JSX.Element {
  return (
    <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, minWidth: 0 }}>
      {showMode && (
        <Segmented
          ariaLabel={t('workspace.mode', 'Mode')}
          value={planMode ? 'plan' : 'act'}
          onChange={(next) => onPlanModeChange(next === 'plan')}
          testId="mode-segmented"
          options={[
            { value: 'act', label: t('workspace.mode.act', 'Act') },
            { value: 'plan', label: t('workspace.mode.plan', 'Plan'), ariaLabel: t('workspace.planMode', 'Plan mode (no changes)') },
          ]}
        />
      )}
      {agentPicker}
      {modelPicker}
      {runLocation}
      {contextMeter}
    </Box>
  );
}

export function WorkspaceComposer(props: WorkspaceComposerProps): React.JSX.Element {
  const { ipc, workspaceId, canSend, busy, planMode, onPlanModeChange, skills, customCommands, onSend, onStop, onCommand } = props;
  // The thread runs in the cloud: the input is off, and `canSend` is false.
  const locked = props.readOnly === true;
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

  const slash = useMemo(() => (open?.kind === 'command' ? composer.slashMenu(open.query, customCommands, skills) : []), [open, customCommands, skills]);
  const items: SuggestionItem[] = useMemo(() => {
    if (open?.kind === 'command') return slash.map(slashItem);
    if (open?.kind === 'file') {
      return (files.data ?? []).map((f) => ({
        key: `${f.kind}:${f.path}`,
        label: f.kind === 'dir' ? `${f.path}/` : f.path,
        icon: f.kind === 'dir' ? <FolderOutlinedIcon fontSize="small" /> : <InsertDriveFileOutlinedIcon fontSize="small" />,
      }));
    }
    return [];
  }, [open, slash, files.data]);
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
      const entry = slash[index];
      if (entry === undefined) return;
      if (entry.kind !== 'host') {
        // A custom command or a skill: the message starts with it, and the caret goes on after it.
        handle?.replaceRange(open.start, open.end, `${insertedToken(entry)} `);
        setToken(null);
        return;
      }
      // Without moving the caret: replaceRange would focus the input again,
      // and a command (`/agent`) may move the focus elsewhere.
      const content = handle?.getInputContent() ?? '';
      handle?.setValue(content.slice(0, open.start) + content.slice(open.end));
      setToken(null);
      onCommand(entry.command.id);
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

  const clearInput = (): void => {
    inputRef.current?.reset();
    setPicked([]);
    setToken(null);
  };

  const send = (question: string): void => {
    const named = props.onModelCommand === undefined ? null : composer.modelCommandName(question);
    if (named !== null) {
      props.onModelCommand?.(named);
      clearInput();
      return;
    }
    const mentions = composer.referencedPaths(question, picked);
    // A custom command first: it hides a skill of its name in the menu too.
    const command = composer.invokedCustomCommand(question, customCommands);
    const skill = command === null ? composer.invokedSkill(question, skills) : null;
    const sending = command === null ? onSend(question, mentions, skill === null ? [] : [skill.name]) : onSend(question, mentions, [], command);
    void sending.then((sent) => {
      if (sent) clearInput();
    });
  };

  return (
    <Box data-testid="workspace-composer" onFocus={props.onFocus} sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
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
        state={{ isStreaming: busy, disabledSend: !canSend, disabledInput: locked }}
        content={{
          placeholder: props.placeholder ?? placeholderFor(locked),
          clearInputAfterSubmit: false,
          tooltipOfSendButton: t('workspace.send', 'Send'),
        }}
        callbacks={{ onSend: send, onStopGeneration: onStop, onNormalKeyDown: onKeyDown, onInputChange: track }}
        agentEditor={NO_AGENT_EDITOR}
        slots={{
          modelSelector: null,
          attachmentButton: <FooterControls planMode={planMode} onPlanModeChange={onPlanModeChange} showMode={props.showMode !== false} agentPicker={props.agentPicker} modelPicker={props.modelPicker} runLocation={props.runLocation} contextMeter={props.contextMeter} />,
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
