import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { resetConfigForTests } from '@/shared/config/get-config';

import {
  canEditParticipant,
  isDetailsLoading,
  isEditSettingsDisabled,
  isPipelineParticipant,
  isPublicParticipant,
  isSelectedVersionPublished,
  resolvePublicProjectId,
  resolveSelectedVersion,
  settingsTooltipTitle,
  showEditLlmSettingsButton,
  switchEntityTooltip,
} from './AgentEditorPanel.derive';

describe('isPipelineParticipant', () => {
  it('is true only for entitySettings.agentType === "pipeline"', () => {
    expect(isPipelineParticipant({ entitySettings: { agentType: 'pipeline' } } as never)).toBe(true);
    expect(isPipelineParticipant({ entitySettings: { agentType: 'chat' } } as never)).toBe(false);
    expect(isPipelineParticipant(undefined)).toBe(false);
  });
});

describe('isDetailsLoading', () => {
  it('is true when there is no name, or the details id does not match the active participant', () => {
    expect(isDetailsLoading(undefined, undefined)).toBe(true);
    expect(isDetailsLoading({ name: '' }, undefined)).toBe(true);
    expect(isDetailsLoading({ id: 'a', name: 'Agent' }, { entityMeta: { id: 'b' } } as never)).toBe(true);
    expect(isDetailsLoading({ id: 'a', name: 'Agent' }, { entityMeta: { id: 'a' } } as never)).toBe(false);
  });
});

describe('resolveSelectedVersion', () => {
  const versions = [
    { id: '1', name: 'base', status: 'draft', agentType: 'chat', createdAt: '2024-01-01' },
    { id: '2', name: 'v2', status: 'published', agentType: 'chat', createdAt: '2024-02-01' },
  ];
  it('finds by id first', () => {
    expect(resolveSelectedVersion(versions, '2')).toEqual(versions[1]);
  });
  it('falls back to the "base" version', () => {
    expect(resolveSelectedVersion(versions, 'missing')).toEqual(versions[0]);
  });
  it('is undefined when neither resolves (no first-item fallback, disclosed)', () => {
    expect(resolveSelectedVersion([versions[1] as (typeof versions)[number]], 'missing')).toBeUndefined();
  });
});

describe('isPublicParticipant', () => {
  it('compares entityMeta.projectId against the given public project id', () => {
    expect(isPublicParticipant({ entityMeta: { projectId: '1' } } as never, '1')).toBe(true);
    expect(isPublicParticipant({ entityMeta: { projectId: '2' } } as never, '1')).toBe(false);
    expect(isPublicParticipant(undefined, '1')).toBe(false);
  });
});

describe('canEditParticipant', () => {
  it('requires both !isPublic and hasEditPermission', () => {
    expect(canEditParticipant(false, true)).toBe(true);
    expect(canEditParticipant(true, true)).toBe(false);
    expect(canEditParticipant(false, false)).toBe(false);
  });
});

describe('isSelectedVersionPublished / isEditSettingsDisabled', () => {
  it('published + not public => settings disabled', () => {
    expect(isSelectedVersionPublished({ status: 'published' } as never)).toBe(true);
    expect(isSelectedVersionPublished({ status: 'draft' } as never)).toBe(false);
    expect(isSelectedVersionPublished(undefined)).toBe(false);

    expect(isEditSettingsDisabled(false, true)).toBe(true);
    expect(isEditSettingsDisabled(true, true)).toBe(false);
    expect(isEditSettingsDisabled(false, false)).toBe(false);
  });
});

describe('settingsTooltipTitle / switchEntityTooltip', () => {
  it('branches on pipeline/published/canEdit', () => {
    expect(settingsTooltipTitle(true, false, true)).toBe('Pipeline settings');
    expect(settingsTooltipTitle(true, false, false)).toBe('View pipeline settings');
    expect(settingsTooltipTitle(false, true, true)).toBe('Published versions are not editable');
    expect(settingsTooltipTitle(false, false, true)).toBe('Agent Settings');
    expect(settingsTooltipTitle(false, false, false)).toBe('View agent Settings');

    expect(switchEntityTooltip(true)).toBe('Switch Pipeline');
    expect(switchEntityTooltip(false)).toBe('Switch Agent');
  });
});

describe('resolvePublicProjectId', () => {
  const ALL_KEYS = ['VITE_SERVER_URL', 'VITE_BASE_URI', 'VITE_SOCKET_SERVER', 'VITE_SOCKET_PATH', 'VITE_PUBLIC_PROJECT_ID'] as const;
  const g = globalThis as unknown as Record<string, unknown>;
  const realProcessEnv = (g['process'] as { env: Record<string, string | undefined> }).env;

  beforeEach(() => {
    resetConfigForTests();
    for (const key of ALL_KEYS) delete realProcessEnv[key];
  });
  afterEach(() => {
    vi.unstubAllEnvs();
    resetConfigForTests();
  });

  it('returns the resolved config value', () => {
    vi.stubEnv('VITE_SERVER_URL', '/api/v2');
    vi.stubEnv('VITE_BASE_URI', '/app/');
    vi.stubEnv('VITE_SOCKET_SERVER', 'http://localhost');
    vi.stubEnv('VITE_SOCKET_PATH', '/socket.io');
    vi.stubEnv('VITE_PUBLIC_PROJECT_ID', 'proj-1');
    expect(resolvePublicProjectId()).toBe('proj-1');
  });

  it('falls back to "" when required config is missing', () => {
    expect(resolvePublicProjectId()).toBe('');
  });
});

describe('showEditLlmSettingsButton (A14, ELITEA-0386)', () => {
  it('is true only when a handler is supplied AND the caller can edit', () => {
    const onEditLlmSettings = () => {};
    expect(showEditLlmSettingsButton(onEditLlmSettings, true)).toBe(true);
    expect(showEditLlmSettingsButton(onEditLlmSettings, false)).toBe(false);
    expect(showEditLlmSettingsButton(undefined, true)).toBe(false);
    expect(showEditLlmSettingsButton(undefined, false)).toBe(false);
  });
});
