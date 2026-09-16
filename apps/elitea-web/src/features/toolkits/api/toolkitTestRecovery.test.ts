import { afterEach, expect, it } from 'vitest';
import { clearNamespace, createStorage } from '@/shared/lib/storage';
import { forgetToolkitTest, recalledToolkitTest, rememberToolkitTest } from './toolkitTestRecovery';

afterEach(() => { createStorage('session').remove('toolkits.pendingTest'); });
it('isolates toolkit context, validates identities, and participates in logout cleanup', () => {
  rememberToolkitTest('7', '19', 'execution-1');
  expect(recalledToolkitTest('7', '19')).toEqual({ taskId: 'execution-1' });
  expect(recalledToolkitTest('8', '19')).toBeUndefined();
  forgetToolkitTest('8', '19');
  expect(recalledToolkitTest('7', '19')).toEqual({ taskId: 'execution-1' });
  clearNamespace();
  expect(recalledToolkitTest('7', '19')).toBeUndefined();
  rememberToolkitTest(undefined, '19', 'execution-1');
  expect(createStorage('session').get('toolkits.pendingTest')).toBeNull();
});
it('ignores corrupt persisted state', () => {
  createStorage('session').setJSON('toolkits.pendingTest', { projectId: '7', toolkitId: '19', taskId: '../../wrong' });
  expect(recalledToolkitTest('7', '19')).toBeUndefined();
});

it('keeps a request receipt without arguments and reads old execution receipts', () => {
  rememberToolkitTest('7', '19', 'request-1', 'request');
  expect(recalledToolkitTest('7', '19')).toEqual({ taskId: 'request-1', lookup: 'request' });
  expect(JSON.parse(createStorage('session').get('toolkits.pendingTest') ?? '{}')).toEqual({ projectId: '7', toolkitId: '19', taskId: 'request-1', lookup: 'request' });
  createStorage('session').setJSON('toolkits.pendingTest', { projectId: '7', toolkitId: '19', taskId: 'execution-1' });
  expect(recalledToolkitTest('7', '19')).toEqual({ taskId: 'execution-1' });
});
