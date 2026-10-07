import { ApiError } from '@/api/errors';
import { isOfflineError } from '@/features/chats/offlineEmpty';

it('only a request that never reached the workspace reads as offline', () => {
  const err = (kind: string) => Object.assign(Object.create(ApiError.prototype) as ApiError, { kind });
  expect(isOfflineError(err('network'))).toBe(true);
  expect(isOfflineError(err('timeout'))).toBe(true);
  expect(isOfflineError(err('http'))).toBe(false);
  expect(isOfflineError(new Error('x'))).toBe(false);
});
