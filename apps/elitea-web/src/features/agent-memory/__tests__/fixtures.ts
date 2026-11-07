import type { AgentMemoryEntry } from '@/shared/api/generated/model';

export function memoryEntry(overrides: Partial<AgentMemoryEntry> = {}): AgentMemoryEntry {
  return {
    id: 7,
    application_id: 41,
    kind: 'preference',
    content: 'Prefers short answers',
    subject_user_id: 3,
    status: 'active',
    pinned: false,
    version: 1,
    conversation_uuid: null,
    message_id: null,
    execution_id: null,
    written_by_kind: 'agent',
    written_by_user_id: null,
    created_at: '2026-11-01T10:00:00Z',
    updated_at: '2026-11-01T10:00:00Z',
    ...overrides,
  };
}
