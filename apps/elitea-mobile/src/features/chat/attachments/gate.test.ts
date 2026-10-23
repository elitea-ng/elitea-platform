import { attachmentGate, gateRefusalSettled, handoffPick, type GateInput } from '@/features/chat/attachments/gate';

const base = { online: true, conversationUuid: 'u-1' };
const agent = (versionStatus: 'loading' | 'error' | 'ready', internalTools: string[] = []): GateInput => ({
  ...base,
  target: { kind: 'agent', versionStatus, internalTools },
});

describe("Home's photo/file hand-over (F2)", () => {
  it('opens the picker when the target takes files', () => {
    expect(handoffPick('files', agent('ready', ['attachments']))).toEqual({ kind: 'pick', source: 'files' });
    expect(handoffPick('photos', { ...base, target: { kind: 'model' } })).toEqual({ kind: 'pick', source: 'photos' });
  });

  it('waits while the gate is still settling (offline, checking the agent, conversation not loaded)', () => {
    expect(handoffPick('files', agent('loading'))).toEqual({ kind: 'wait' });
    expect(handoffPick('files', { ...agent('ready', ['attachments']), online: false })).toEqual({ kind: 'wait' });
    expect(handoffPick('files', { ...agent('ready'), conversationUuid: null })).toEqual({ kind: 'wait' });
  });

  it('says why instead of spending the hand-over silently when the agent takes no files or could not be read', () => {
    const plan = handoffPick('files', agent('ready'));
    expect(plan).toEqual({ kind: 'explain', title: "Can't attach a file here", message: expect.stringContaining("This agent doesn't take files.") });
    expect(handoffPick('photos', agent('error'))).toMatchObject({ kind: 'explain', title: "Can't attach a photo here" });
  });

  it('does nothing for dictation or no hand-over', () => {
    expect(handoffPick('dictate', agent('ready'))).toEqual({ kind: 'none' });
    expect(handoffPick(null, agent('ready'))).toEqual({ kind: 'none' });
  });

  it('a settled refusal is one the screen cannot outwait', () => {
    expect(gateRefusalSettled(agent('ready'))).toBe(true);
    expect(gateRefusalSettled(agent('ready', ['attachments']))).toBe(false);
    expect(gateRefusalSettled({ ...agent('ready'), online: false })).toBe(false);
    expect(attachmentGate({ ...agent('ready'), online: false })).toBe('Attachments need a connection.');
  });
});
