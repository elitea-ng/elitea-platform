/**
 * May this conversation's next message carry files? Fails closed: the server
 * does not check an agent's `attachments` internal tool, so the app must.
 */
export interface GateInput {
  online: boolean;
  /** The conversation's uuid (uploads are keyed by it). */
  conversationUuid: string | null;
  target:
    | { kind: 'agent'; versionStatus: 'loading' | 'error' | 'ready'; internalTools: readonly string[] }
    | { kind: 'model' }
    | { kind: 'none' };
}

export function attachmentGate(input: GateInput): string | null {
  if (!input.online) return 'Attachments need a connection.';
  if (!input.conversationUuid) return 'Files cannot be attached to this conversation from mobile.';
  switch (input.target.kind) {
    case 'none':
      return 'Files cannot be attached to this conversation from mobile.';
    case 'model':
      return null;
    case 'agent':
      if (input.target.versionStatus === 'loading') return 'Checking whether this agent takes files…';
      if (input.target.versionStatus === 'error') return "Couldn't check whether this agent takes files.";
      return input.target.internalTools.includes('attachments') ? null : "This agent doesn't take files.";
  }
}

/**
 * Whether a refusal is final for this screen: the device is online, the conversation is known and
 * the agent's version has been read (it takes no files, or it could not be read). Offline and
 * "checking" refusals clear by themselves, so a hand-over waits for them instead.
 */
export function gateRefusalSettled(input: GateInput): boolean {
  if (!input.online || !input.conversationUuid) return false;
  return input.target.kind === 'agent' && input.target.versionStatus !== 'loading' && attachmentGate(input) !== null;
}

/**
 * What Home's "Ask about a photo/file" hand-over does once the thread is open (F2): open the
 * picker when files may be attached, wait while the gate is still settling, or say why not when it
 * refused for good, so the one-time action is never spent silently.
 */
export function handoffPick(
  then: 'dictate' | 'photos' | 'files' | null,
  input: GateInput,
): { kind: 'none' } | { kind: 'wait' } | { kind: 'pick'; source: 'photos' | 'files' } | { kind: 'explain'; title: string; message: string } {
  if (then !== 'photos' && then !== 'files') return { kind: 'none' };
  const reason = attachmentGate(input);
  if (reason === null) return { kind: 'pick', source: then };
  if (!gateRefusalSettled(input)) return { kind: 'wait' };
  return {
    kind: 'explain',
    title: then === 'photos' ? "Can't attach a photo here" : "Can't attach a file here",
    message: `${reason} Start a new chat with an agent or a model that takes files.`,
  };
}
