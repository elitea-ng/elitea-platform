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
