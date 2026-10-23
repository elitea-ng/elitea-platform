import { reportActionFailure, subscribeActionFailures, type FailedOp } from '@/sync/actionFailures';

// One owner tells a refused action: the newest listener that takes it.

const unarchive: FailedOp = { projectId: 7, conversationId: 21, action: { kind: 'archive', archived: false } };
const pin: FailedOp = { projectId: 7, conversationId: 21, action: { kind: 'pin', pinned: true } };

describe('reportActionFailure', () => {
  it('offers the newest listener first; one that takes it ends it', () => {
    const chats = jest.fn(() => true);
    const archived = jest.fn((op: FailedOp) => op.action.kind === 'archive' && !op.action.archived);
    const offChats = subscribeActionFailures(chats);
    const offArchived = subscribeActionFailures(archived);

    reportActionFailure(unarchive);
    expect(archived).toHaveBeenCalledTimes(1);
    expect(chats).not.toHaveBeenCalled();

    // Not the newer screen's: passed on.
    reportActionFailure(pin);
    expect(chats).toHaveBeenCalledWith(pin);

    // The newer screen closed: the older one takes everything.
    offArchived();
    reportActionFailure(unarchive);
    expect(chats).toHaveBeenCalledTimes(2);
    expect(archived).toHaveBeenCalledTimes(2);
    offChats();
  });

  it('a listener that throws does not stop the others', () => {
    jest.spyOn(console, 'warn').mockImplementation(() => {});
    const older = jest.fn(() => true);
    const offOlder = subscribeActionFailures(older);
    const offBroken = subscribeActionFailures(() => {
      throw new Error('boom');
    });
    reportActionFailure(pin);
    expect(older).toHaveBeenCalledTimes(1);
    offBroken();
    offOlder();
    jest.restoreAllMocks();
  });
});
