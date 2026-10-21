/**
 * Which approval layer decided a call, as a person reads it: "workspace rule
 * 3", "your rule 1", "the defaults" (#1216). `rule` is the host's label
 * (`workspace#3`, `user#1`, `legacy#2` for a rule migrated from an older
 * host's workspace settings); `source` is the layer (`policy`, `plan_mode`,
 * `workspace`, `user`, `remembered`, `default`).
 */
import { t } from '@/shared/i18n';

const RULE_LABELS: Record<string, (n: number) => string> = {
  workspace: (n) => t('workspace.rules.sourceWorkspaceRule', 'workspace rule {{n}}', { n }),
  user: (n) => t('workspace.rules.sourceUserRule', 'your rule {{n}} (every workspace)', { n }),
  legacy: (n) => t('workspace.rules.sourceLegacyRule', 'workspace setting {{n}} (from an earlier version)', { n }),
};

export function ruleSourceLabel(source: string | undefined, rule?: string | null): string | undefined {
  const match = rule == null ? null : /^(workspace|user|legacy)#(\d+)$/.exec(rule);
  if (match !== null) return RULE_LABELS[match[1]!]?.(Number(match[2]));
  const layers: Record<string, () => string> = {
    policy: () => t('workspace.rules.sourcePolicy', 'your organisation’s policy'),
    plan_mode: () => t('workspace.rules.sourcePlanMode', 'plan mode'),
    workspace: () => t('workspace.rules.sourceWorkspace', 'a workspace rule'),
    user: () => t('workspace.rules.sourceUser', 'one of your rules'),
    remembered: () => t('workspace.rules.sourceRemembered', 'an earlier “always allow”'),
    default: () => t('workspace.rules.sourceDefault', 'the defaults'),
  };
  return source === undefined ? undefined : layers[source]?.();
}
