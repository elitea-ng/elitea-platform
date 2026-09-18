import { useGetPlatformSettings } from '@/shared/api/generated/admin/admin';
import type { PlatformSettings } from '@/shared/api/generated/model';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/shared/lib/hooks/
 * useMcpVisibility.hooks.js`'s `useIsMcpVisible`.
 *
 * **Deliberate 5th duplicate, not a promotion** — this exact hook already
 * exists feature-locally four times (`features/agents/api/useIsMcpVisible
 * .ts`, `features/toolkits/api/useIsMcpVisible.ts`, `features/pipelines/api
 * /useIsMcpVisible.ts`, `features/chat-input/lib/hooks/useIsMcpVisible.ts`),
 * each barred by `no-sideways-features` from importing any of the others.
 * Same call here: this is what closes elitea_issues #5367's adjacent finding
 * — `Participants.tsx`'s own grouping already buckets an mcp-typed toolkit
 * participant into its `'mcp'` group correctly (local/stdio and remote
 * alike), but then drops that entire group unless `isMcpVisible` is `true`,
 * and NOTHING upstream of `ParticipantsWrapper` was ever resolving that flag
 * — `pages/chat/index.tsx`'s `<ParticipantsWrapper>` call site passed no
 * `isMcpVisible` prop at all, so every mcp-classified participant was
 * invisible in the panel regardless of the platform's actual MCP-visibility
 * setting. See `S/issues/gaps.md`'s C-chat section for the full trace.
 *
 * See `features/agents/api/useIsMcpVisible.ts` for the two-flag rule this
 * implements and why the platform-settings contract gap it used to document
 * is closed (A14, issue #200).
 */
export function useIsMcpVisible(): boolean {
  const query = useGetPlatformSettings();
  const platformSettings = query.data?.data as PlatformSettings | undefined;
  return platformSettings?.mcp_enabled !== false && platformSettings?.mcp_in_menu_enabled !== false;
}
