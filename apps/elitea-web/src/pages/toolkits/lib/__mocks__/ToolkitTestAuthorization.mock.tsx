import { vi } from 'vitest';

const mocks = vi.hoisted(() => ({ reference: vi.fn(), login: vi.fn() }));
export { mocks };

const finishLabel = 'Finish OAuth';
const cancelLabel = 'Cancel OAuth';

vi.mock('@/features/mcps', () => ({
  getAuthorizationReference: mocks.reference,
  useMcpLogin: (options: { onSuccess: () => void }) => ({ onLogin: mocks.login, modalProps: { onClose: () => options.onSuccess(), onCancel: vi.fn() } }),
  McpAuthModal: ({ onClose, onCancel }: { onClose: () => void; onCancel: () => void }) => <><button onClick={onClose}>{finishLabel}</button><button onClick={onCancel}>{cancelLabel}</button></>,
}));
