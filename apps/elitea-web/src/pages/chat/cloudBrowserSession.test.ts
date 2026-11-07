import { describe, expect, it } from 'vitest';

import { browserSessionNameFor } from '@/shared/lib/browserSessionName';

import { cloudBrowserSessionOf } from './cloudBrowserSession';

const browserCall = (name: string, toolkitType?: string) => ({ type: 'tool', name, ...(toolkitType ? { toolMeta: { toolkit_type: toolkitType } } : {}) });

describe('cloudBrowserSessionOf', () => {
  it("is the session of the newest answer whose run used the browser", () => {
    const messages = [
      { role: 'user', content: 'find shoes' },
      { role: 'assistant', taskId: 'exec-old', toolActions: [browserCall('browser_open')] },
      { role: 'assistant', taskId: 'exec-new', toolActions: [browserCall('click_thing', 'browser')] },
      { role: 'assistant', taskId: 'exec-none', toolActions: [browserCall('search')] },
    ];
    expect(cloudBrowserSessionOf(messages)).toBe(browserSessionNameFor('exec-new'));
  });

  it('is nothing without a browser call, or without the run id', () => {
    expect(cloudBrowserSessionOf([{ role: 'assistant', taskId: 'e', toolActions: [browserCall('search')] }])).toBeUndefined();
    expect(cloudBrowserSessionOf([{ role: 'assistant', toolActions: [browserCall('browser_snapshot')] }])).toBeUndefined();
    expect(cloudBrowserSessionOf([{ role: 'assistant', taskId: '../x', toolActions: [browserCall('browser_snapshot')] }])).toBeUndefined();
    expect(cloudBrowserSessionOf([])).toBeUndefined();
  });
});
