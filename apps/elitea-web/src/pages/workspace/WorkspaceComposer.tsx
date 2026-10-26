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
 *   `/steps <number|unlimited|default>` sets the folder's step limit
 *   (`onStepsCommand`); `/steps` alone shows it.
 *
 * Both menus are driven from the keyboard without leaving the textarea:
 * ↑/↓ move, Enter or Tab pick, Esc closes until the token changes.
 *
 * Images (#1229): a pasted image, or an image file dropped on the window
 * (the host reports its path; the shell hands it to `attachFiles`), is
 * registered with the host (`attachBytes` / `attachFile`), which checks and
 * shrinks it and answers a token and a thumbnail. The thumbnails sit above
 * the input with a remove button each; Send carries the tokens. The host
 * enforces the limits (five images, 10 MiB each); this only stops offering.
 *
 * Steering (`onSteer`): while a turn runs, the input stays open and Send queues the text against the turn
 * (images stay for the next turn). "Stop and send" (`onStopAndSend`) stops the turn and sends the text next.
 */
import { useMemo, useRef, useState, type ComponentRef, type KeyboardEvent, type ReactNode } from 'react';

import { useQuery } from '@tanstack/react-query';

import FolderOutlinedIcon from '@mui/icons-material/FolderOutlined';
import InsertDriveFileOutlinedIcon from '@mui/icons-material/InsertDriveFileOutlined';
import SendIcon from '@mui/icons-material/Send';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { NewChatInput } from '@/features/chat-input';
import { composer, SuggestionMenu, type SuggestionItem, type WorkspaceCommandId } from '@/features/workspace';

import { FooterControls } from './FooterControls';
import type { ComposerSkill } from './useAgentSkills';
import { useImageAttachments } from './useImageAttachments';
import type { SentImages } from './useSendPrompt';
import type { CustomCommand, TurnCommand, TurnStartRequest, WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

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
  /** Bypass mode is on (`useBypassMode`): the mode switch shows Bypass. */
  bypass?: boolean;
  /** Offers Bypass in the mode switch (a local thread only): `true` when it is picked, `false` when Act or Plan is. */
  onBypassChange?: ((on: boolean) => void) | undefined;
  /** The footer's agent·version picker and run-location chip (`ComposerControls`). */
  agentPicker?: ReactNode;
  /** A thread in the cloud: the placeholder says who the message goes to. */
  placeholder?: string | undefined;
  /** Act | Plan | Bypass: off for a cloud thread, whose turns have no plan mode. */
  showMode?: boolean;
  runLocation?: ReactNode;
  /** The meters (the language-server and usage chips), after the run location. */
  contextMeter?: ReactNode;
  /** The model chip (`ModelChip`), after the agent picker. */
  modelPicker?: ReactNode;
  onPlanModeChange: (planMode: boolean) => void;
  /** The selected agent's skills, then the project's (`project`), offered after the commands in the "/" menu. */
  skills: readonly ComposerSkill[];
  /** The custom commands (`commands_list`), offered after the host commands; none for a cloud thread. */
  customCommands: readonly CustomCommand[];
  /**
   * Resolves `true` once the turn started (the input is then cleared). `skills`: the skill names the message invokes;
   * `command`: the custom command it runs, if any (then no skill).
   */
  onSend: (prompt: string, mentions: string[], skills: TurnStartRequest['skills'], command?: TurnCommand, images?: SentImages) => Promise<boolean>;
  /** Images can be attached (a local thread; not the cloud copy). Default true. */
  acceptImages?: boolean;
  onStop: () => void;
  onCommand: (command: WorkspaceCommandId) => void;
  /** `/model <name>` was sent as the whole message: the page sets the model (nothing goes to the agent). */
  onModelCommand?: ((name: string) => void) | undefined;
  /** `/steps <value>` was sent as the whole message: the page sets the folder's step limit (nothing goes to the agent). */
  onStepsCommand?: ((command: StepsCommand) => void) | undefined;
  /** The composer got focus: the page lets the host read ahead for the send (#1213). */
  onFocus?: () => void;
  /** While `busy`: Send queues the text against the running turn; resolves `true` once queued (the input clears). */
  onSteer?: ((text: string) => Promise<boolean>) | undefined;
  /** While `busy`: stop the turn, then send the text as the next turn. */
  onStopAndSend?: ((text: string) => Promise<boolean>) | undefined;
}

/** What `/steps <value>` asks for (`composer.stepsCommand`). */
export type StepsCommand = NonNullable<ReturnType<typeof composer.stepsCommand>>;

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
function placeholderFor(readOnly: boolean, steering: boolean): string {
  if (readOnly) return t('workspace.composer.cloudPlaceholder', 'This thread is running in the cloud. Take it back to continue here.');
  return steering
    ? t('workspace.steer.placeholder', 'Add a message for the agent. It reads it after its current step.')
    : t('workspace.composer.placeholder', 'What should the agent do? Type @ to add a file, / for commands and skills');
}

/** Send steers the running turn: the page offers it and a turn runs. */
const steersNow = (busy: boolean, onSteer: WorkspaceComposerProps['onSteer']): boolean => busy && onSteer !== undefined;

/** "Stop and send": shown while a turn runs, when the page offers it (`shown`). */
function StopAndSend({ shown, disabled, onClick }: { shown: boolean; disabled: boolean; onClick: () => void }): React.JSX.Element | null {
  if (!shown) return null;
  return (
    <Box sx={{ display: 'flex', justifyContent: 'flex-end' }}>
      <Button size="small" variant="outlined" color="error" data-testid="workspace-stop-and-send" disabled={disabled} onClick={onClick}>
        {t('workspace.steer.stopAndSend', 'Stop and send')}
      </Button>
    </Box>
  );
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
        key: `skill:${entry.skill.project === true ? 'project' : 'version'}:${entry.skill.id}`,
        label: composer.skillToken(entry.skill),
        section: entry.skill.project === true ? t('workspace.composer.projectSkills', 'Project skills') : t('workspace.composer.skills', 'Skills'),
        ...(entry.skill.description !== undefined ? { description: entry.skill.description } : {}),
      };
  }
}

/** What the start request names for the skill a message invokes: a version skill by name, a project skill by id. */
/** The start request's `skills` for the skill a message invokes. */
export function skillPicks(skill: ComposerSkill | null): TurnStartRequest['skills'] {
  if (skill === null) return [];
  return [skill.project === true ? { source: 'project', id: skill.id } : skill.name];
}

/** What picking a custom command or a skill inserts. */
function insertedToken(entry: Exclude<SlashEntry, { kind: 'host' }>): string {
  return entry.kind === 'custom' ? composer.customCommandToken(entry.command) : composer.skillToken(entry.skill);
}

/** Images can be attached to a local thread that is not read only. */
const takesImages = (accept: boolean | undefined, locked: boolean): boolean => accept !== false && !locked;

const sameToken = (a: Token, b: Token): boolean => a?.kind === b?.kind && a?.start === b?.start && a?.query === b?.query;

export function WorkspaceComposer(props: WorkspaceComposerProps): React.JSX.Element {
  const { ipc, workspaceId, canSend, busy, planMode, onPlanModeChange, skills, customCommands, onSend, onStop, onCommand } = props;
  // The thread runs in the cloud: the input is off, and `canSend` is false.
  const locked = props.readOnly === true;
  const steering = steersNow(busy, props.onSteer);
  const [hasDraft, setHasDraft] = useState(false);
  const inputRef = useRef<ComposerHandle>(null);
  const [token, setToken] = useState<Token>(null);
  const [dismissed, setDismissed] = useState<Token>(null);
  const [activeIndex, setActiveIndex] = useState(0);
  // What was picked with "@", in pick order (a folder keeps its trailing "/").
  const [picked, setPicked] = useState<string[]>([]);
  const attachments = useImageAttachments(ipc, workspaceId, takesImages(props.acceptImages, locked));

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
    setHasDraft(value.trim() !== '');
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
    setHasDraft(false);
    setPicked([]);
    setToken(null);
  };

  const send = (question: string): void => {
    const steps = props.onStepsCommand === undefined ? null : composer.stepsCommand(question);
    if (steps !== null) {
      props.onStepsCommand?.(steps);
      clearInput();
      return;
    }
    const named = props.onModelCommand === undefined ? null : composer.modelCommandName(question);
    if (named !== null) {
      props.onModelCommand?.(named);
      clearInput();
      return;
    }
    if (steering) {
      // A steering message is plain text: the images stay for the next turn.
      void props.onSteer?.(question).then((queued) => queued && clearInput());
      return;
    }
    const mentions = composer.referencedPaths(question, picked);
    // The longest name the message starts with; a custom command over a skill of its name.
    const { command, skill } = composer.invokedPick(question, customCommands, skills);
    const images = attachments.sent();
    const sending = command === null ? onSend(question, mentions, skillPicks(skill), undefined, images) : onSend(question, mentions, [], command, images);
    void sending.then((sent) => {
      if (!sent) return;
      clearInput();
      attachments.clear();
    });
  };

  const stopAndSend = (): void => {
    const text = inputRef.current?.getInputContent() ?? '';
    if (text.trim() !== '') void props.onStopAndSend?.(text).then((sent) => sent && clearInput());
  };

  return (
    <Box data-testid="workspace-composer" onFocus={props.onFocus} onPasteCapture={attachments.onPaste} sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
      <composer.ImageStrip images={attachments.images} onRemove={attachments.remove} />
      {attachments.error !== null && (
        <Typography variant="bodySmall" role="alert" data-testid="attachment-error" sx={(theme: Theme) => ({ color: theme.vars.palette.error.main })}>
          {attachments.error}
        </Typography>
      )}
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
      <StopAndSend shown={steersNow(busy, props.onStopAndSend)} disabled={!hasDraft} onClick={stopAndSend} />
      <NewChatInput
        ref={inputRef}
        state={{ isStreaming: busy, disabledSend: !canSend, disabledInput: locked, allowSendWhileStreaming: steering }}
        content={{
          placeholder: props.placeholder ?? placeholderFor(locked, steering),
          clearInputAfterSubmit: false,
          tooltipOfSendButton: t('workspace.send', 'Send'),
        }}
        callbacks={{ onSend: send, onStopGeneration: onStop, onNormalKeyDown: onKeyDown, onInputChange: track }}
        agentEditor={NO_AGENT_EDITOR}
        slots={{
          modelSelector: null,
          attachmentButton: <FooterControls planMode={planMode} onPlanModeChange={onPlanModeChange} bypass={props.bypass === true} onBypassChange={props.onBypassChange} showMode={props.showMode !== false} agentPicker={props.agentPicker} modelPicker={props.modelPicker} runLocation={props.runLocation} contextMeter={props.contextMeter} />,
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
