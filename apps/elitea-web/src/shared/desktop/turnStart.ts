/**
 * What `agent_turn_start` carries, and the desktop composer's "/" menu as
 * the host lists it (IPC.md "Custom commands", issue 1232): the project
 * skill and custom command picks, a `commands_list` row. Wire shapes are
 * the host's (snake_case).
 */
import type { TurnModelChoice } from './turnModel';

export interface TurnStartRequest {
  workspace_id: string;
  project_id: number;
  conversation_id: string;
  application_id: number;
  version_id: number;
  prompt: string;
  plan_mode: boolean;
  /** Workspace-relative paths the person referenced with "@" (a folder ends with `/`); the host checks them and lists them under the prompt. */
  mentions: string[];
  /**
   * The skills the person picked with "/": the agent version's by name (resolved against the version), the project's
   * as `{source: "project", id}` (read by the host itself); applied to this turn.
   */
  skills: (string | ProjectSkillPick)[];
  /** `/compact`: the host compacts the conversation's context at the turn's first model call (IPC.md "Context window"). */
  compact?: boolean;
  /** Another chat model of the project for this turn and the thread's next ones (IPC.md "Model per turn"). */
  model?: TurnModelChoice | undefined;
  /** A custom command the person picked with "/": the host reads and expands its file itself, and records the prompt as `/<name> <arguments>`. */
  command?: TurnCommand | undefined;
  /** Images attached to the message (IPC.md "Image input"), by the tokens `attachment_add` / `attachment_add_bytes` answered; at most five. */
  images?: TurnImagePick[] | undefined;
}

/** `agent_turn_start.images[]`: where the image came from, and its token. */
export interface TurnImagePick {
  source: 'clipboard' | 'file';
  token: string;
}

/** The most images one message carries (the host enforces it too). */
export const MAX_TURN_IMAGES = 5;

/** `attachment_add` / `attachment_add_bytes`' answer: an image the host holds for the next turn of the workspace. */
export interface AttachedImage {
  token: string;
  name: string;
  mime: string;
  /** Size after the host's shrinking. */
  bytes: number;
  width: number;
  height: number;
  /** `data:image/jpeg;base64,…`, a small preview (absent when it could not be made). */
  thumbnail: string | null;
}

/** What a recorded turn keeps of an attached image (`thread_history`): never the image, a thumbnail. */
export interface StoredImage {
  name: string;
  mime: string;
  bytes: number;
  thumbnail: string | null;
}

/** A project skill picked in the "/" menu, by its id only. */
interface ProjectSkillPick {
  source: 'project';
  id: string;
}

/** `agent_turn_start.command`: the command's name as `commands_list` gave it, and what follows it in the message. */
export interface TurnCommand {
  name: string;
  arguments: string;
}

/** One row of `commands_list`: a custom "/" command of the workspace (`.elitea/commands`, `.claude/commands`) or of the person's own folder. */
export interface CustomCommand {
  /** `name`, or `folder:name` for a file in a sub-folder. */
  name: string;
  description: string | null;
  argument_hint: string | null;
  source: 'workspace' | 'user';
  /** Workspace-relative, or relative to the person's folder. */
  path: string;
  /** What the file asks for that the host ignores (`allowed-tools`, `model`, `! shell lines`, `@ file includes`). */
  ignored: string[];
}
