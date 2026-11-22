import { describe, expect, it } from 'vitest';

import { NESTING_REASON, PIPELINE_HOST_TIER, SWARM_REASON } from './agentNesting.helpers';
import {
  DUPLICATE_APPLICATION_NAME_REASON,
  DUPLICATE_NAME_REASON,
  INTERRUPT_REASON,
  buildApplicationPickerItems,
  buildToolkitPickerItems,
} from './pickerItems.helpers';

describe('pickerItems.helpers', () => {
  describe('buildToolkitPickerItems', () => {
    it('pins in-use attached toolkits first and disables duplicate names from rows', () => {
      const items = buildToolkitPickerItems({
        attachedTools: [{ id: 1, type: 'github', name: 'GitHub', toolkit_name: 'gh' }],
        rows: [
          { id: 1, type: 'github', name: 'GitHub', toolkit_name: 'gh' },
          { id: 2, type: 'github', name: 'GitHub Duplicate', toolkit_name: 'gh' },
          { id: 3, type: 'jira', name: 'Jira', toolkit_name: 'jira' },
        ],
        usedNames: new Set(['gh']),
      });

      expect(items).toHaveLength(3);
      expect(items[0]).toMatchObject({ id: 1, inUse: true, isAttached: true, disabledReason: '' });
      expect(items[1]).toMatchObject({
        id: 2,
        inUse: false,
        disabledReason: DUPLICATE_NAME_REASON('gh'),
      });
      expect(items[2]).toMatchObject({ id: 3, inUse: false, disabledReason: '' });
    });

    it('filters pinned items by search query', () => {
      const items = buildToolkitPickerItems({
        attachedTools: [{ id: 1, type: 'github', name: 'GitHub', toolkit_name: 'gh' }],
        rows: [],
        usedNames: new Set(['gh']),
        query: 'jira',
      });
      expect(items).toEqual([]);
    });
  });

  describe('buildApplicationPickerItems (EL-7085)', () => {
    it('excludes current pipeline id, pins in-use attachments, and disables invalid candidates', () => {
      const items = buildApplicationPickerItems({
        excludeId: 100,
        hostTier: PIPELINE_HOST_TIER,
        attachedTools: [
          {
            type: 'application',
            name: 'PinnedAgent',
            settings: { application_id: 10 },
          },
        ],
        allAttachedTools: [
          {
            type: 'application',
            name: 'PinnedAgent',
            settings: { application_id: 10 },
          },
        ],
        usedNames: new Set(['PinnedAgent']),
        rows: [
          { id: 100, name: 'SelfPipeline' },
          { id: 10, name: 'PinnedAgent' },
          { id: 11, name: 'SwarmBot', has_swarm: true },
          { id: 12, name: 'DeepBot', agent_subtree_tiers: 4, max_agent_nesting_tiers: 3 },
          { id: 13, name: 'InterruptPipe', has_interrupt: true },
          { id: 14, name: 'PinnedAgent' },
          { id: 15, name: 'ValidAgent' },
        ],
      });

      expect(items.map((i) => i.id)).toEqual([10, 11, 12, 13, 14, 15]);
      expect(items[0]).toMatchObject({ inUse: true, isAttached: true, disabledReason: '' });
      expect(items[1]?.disabledReason).toBe(SWARM_REASON('SwarmBot'));
      expect(items[2]?.disabledReason).toBe(NESTING_REASON('DeepBot', 3));
      expect(items[3]?.disabledReason).toBe(INTERRUPT_REASON('InterruptPipe'));
      expect(items[4]?.disabledReason).toBe(DUPLICATE_APPLICATION_NAME_REASON('PinnedAgent'));
      expect(items[5]?.disabledReason).toBe('');
    });
  });
});
