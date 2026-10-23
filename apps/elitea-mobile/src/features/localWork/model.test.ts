import {
  canEraseCloudCopy,
  canRequestReclaim,
  composerBlockedReason,
  describeLocalWork,
  executorBadge,
  executorOf,
  executorOfState,
  folderNameOf,
  isLocalWork,
  lastAnswerRanOnDesktop,
} from '@/features/localWork/model';

describe('executorOf / executorOfState', () => {
  it('reads cloud and returning, and everything else as the laptop', () => {
    expect(executorOf({ workspace_executor: 'cloud' })).toBe('cloud');
    expect(executorOf({ workspace_executor: 'returning' })).toBe('returning');
    expect(executorOf({ workspace_executor: 'local' })).toBe('local');
    expect(executorOf({ workspace_executor: 'something new' })).toBe('local');
    expect(executorOf({})).toBe('local');
    expect(executorOf(null)).toBe('local');
  });

  it('maps handoff states as the server does (a send still in flight is the laptop`s)', () => {
    expect(executorOfState('CLOUD_IDLE')).toBe('cloud');
    expect(executorOfState('CLOUD_RUNNING')).toBe('cloud');
    expect(executorOfState('RECLAIMING')).toBe('returning');
    expect(executorOfState('HANDING_OFF')).toBe('local');
    expect(executorOfState('LOCAL')).toBe('local');
    expect(executorOfState('NEW_STATE')).toBeNull();
    expect(executorOfState(undefined)).toBeNull();
  });
});

describe('badge mapping', () => {
  it('names where the thread runs', () => {
    expect(executorBadge('cloud')).toBe('In cloud');
    expect(executorBadge('returning')).toBe('Returning');
    expect(executorBadge('local')).toBe('On laptop');
  });
});

describe('isLocalWork / folderNameOf', () => {
  it('knows a Local work thread by its source, else by the meta the desktop writes', () => {
    expect(isLocalWork({ source: 'local_work' })).toBe(true);
    expect(isLocalWork({ meta: { local_work: { folder_name: 'api' } } })).toBe(true);
    expect(isLocalWork({ source: 'chat', meta: {} })).toBe(false);
    expect(isLocalWork(undefined)).toBe(false);
  });
  it('reads the folder name, never a blank one', () => {
    expect(folderNameOf({ local_work: { folder_name: ' api ' } })).toBe('api');
    expect(folderNameOf({ local_work: { folder_name: '  ' } })).toBeUndefined();
    expect(folderNameOf({})).toBeUndefined();
    expect(folderNameOf(null)).toBeUndefined();
  });
});

describe('lastAnswerRanOnDesktop', () => {
  it('looks at the newest answer only', () => {
    expect(lastAnswerRanOnDesktop([{ role: 'assistant', executedBy: 'desktop' }, { role: 'user' }])).toBe(true);
    expect(lastAnswerRanOnDesktop([{ role: 'assistant', executedBy: 'desktop' }, { role: 'user' }, { role: 'assistant', executedBy: 'cloud' }])).toBe(false);
    expect(lastAnswerRanOnDesktop([{ role: 'assistant' }])).toBe(false);
    expect(lastAnswerRanOnDesktop([{ role: 'user' }])).toBe(false);
    expect(lastAnswerRanOnDesktop([])).toBe(false);
  });
});

describe('thread rules', () => {
  const detail = { source: 'local_work', created_by: '8', meta: { local_work: { folder_name: 'api' } } };
  const owner = (executor: 'local' | 'cloud' | 'returning') => describeLocalWork(detail, executor, 8)!;
  const other = (executor: 'local' | 'cloud' | 'returning') => describeLocalWork(detail, executor, 9)!;

  it('is null for an ordinary chat', () => {
    expect(describeLocalWork({ source: 'chat', created_by: '8' }, 'local', 8)).toBeNull();
  });

  it('the composer works only while the cloud runs the thread', () => {
    expect(composerBlockedReason(null)).toBeNull();
    expect(composerBlockedReason(owner('cloud'))).toBeNull();
    expect(composerBlockedReason(owner('returning'))).toBe('Your laptop is taking this thread back.');
    expect(composerBlockedReason(owner('local'))).toBe('This thread runs on api on your laptop. Send it to the cloud from the desktop app to continue here.');
    expect(composerBlockedReason(describeLocalWork({ source: 'local_work', created_by: '8' }, 'local', 8))).toBe(
      'This thread runs on your laptop. Send it to the cloud from the desktop app to continue here.',
    );
  });

  it('Take back is the owner`s alone, and only while the cloud has the thread', () => {
    expect(canRequestReclaim(owner('cloud'))).toBe(true);
    expect(canRequestReclaim(other('cloud'))).toBe(false);
    expect(canRequestReclaim(owner('returning'))).toBe(false);
    expect(canRequestReclaim(owner('local'))).toBe(false);
    expect(canRequestReclaim(describeLocalWork(detail, 'cloud', null)!)).toBe(false);
  });

  it('Erase is the owner`s or a project settings editor`s, never on a laptop thread', () => {
    expect(canEraseCloudCopy(owner('cloud'), false)).toBe(true);
    expect(canEraseCloudCopy(owner('returning'), false)).toBe(true);
    expect(canEraseCloudCopy(other('cloud'), false)).toBe(false);
    expect(canEraseCloudCopy(other('cloud'), true)).toBe(true);
    expect(canEraseCloudCopy(owner('local'), true)).toBe(false);
  });
});
