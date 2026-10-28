import { renderHook, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';

import { installCompactionIpc } from '@/shared/desktop/compactionSettings';
import { createFakeCompactionIpc } from '@/shared/desktop/compactionSettings.fake';
import { createStorage } from '@/shared/lib/storage';

import { useIdleHold } from './useIdleHold';

type Props = { workspaceId: string; conversationId: string; local: boolean };

function setup(initial: Props = { workspaceId: 'w1', conversationId: 'c1', local: true }) {
  const ipc = createFakeCompactionIpc();
  installCompactionIpc(ipc);
  const hook = renderHook((props: Props) => useIdleHold(props.workspaceId, props.conversationId, props.local), { initialProps: initial });
  const holds = () => ipc.calls.holds.map((call) => `${call.conversationId}:${String(call.hold)}`);
  return { ipc, hook, holds };
}

describe('useIdleHold', () => {
  afterEach(() => installCompactionIpc(undefined));

  it('holds when the draft fills and releases when it empties, only on transitions', () => {
    const { hook, holds } = setup();
    hook.result.current(false);
    expect(holds()).toEqual([]);
    hook.result.current(true);
    hook.result.current(true);
    expect(holds()).toEqual(['c1:true']);
    // A send clears the draft.
    hook.result.current(false);
    hook.result.current(false);
    expect(holds()).toEqual(['c1:true', 'c1:false']);
  });

  it('releases on unmount and when the thread changes', () => {
    const { hook, holds } = setup();
    hook.result.current(true);
    hook.rerender({ workspaceId: 'w1', conversationId: 'c2', local: true });
    expect(holds()).toEqual(['c1:true', 'c1:false']);
    hook.result.current(true);
    hook.unmount();
    expect(holds()).toEqual(['c1:true', 'c1:false', 'c2:true', 'c2:false']);
  });

  it('holds nothing for a new thread or the cloud copy', () => {
    const { hook, holds } = setup({ workspaceId: 'w1', conversationId: '', local: true });
    hook.result.current(true);
    hook.rerender({ workspaceId: 'w1', conversationId: 'c1', local: false });
    hook.result.current(true);
    expect(holds()).toEqual([]);
  });

  it('moves the old idle switch to the host', async () => {
    createStorage('local').set('desktop.idleCompaction.off', '1');
    const { ipc } = setup();
    await waitFor(() => expect(ipc.current().idle).toBe('off'));
    expect(createStorage('local').get('desktop.idleCompaction.off')).toBeNull();
  });
});
