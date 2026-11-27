import { formatDistanceToNow } from 'date-fns';

import { t } from '@/shared/i18n';

/**
 * A row's last change as "Updated 3 days ago" (the Agents and Teams cards,
 * issues 3133 and 3134), or undefined for a missing, unparseable or zero
 * time. Go's zero time ("0001-01-01T00:00:00Z") is what a row answered
 * before its table had the column, and must not read "about 2025 years ago".
 */
export function updatedAgoLabel(updatedAt: string | undefined): string | undefined {
  if (updatedAt === undefined) return undefined;
  const parsed = new Date(updatedAt);
  if (Number.isNaN(parsed.getTime()) || parsed.getUTCFullYear() < 2000) return undefined;
  return t('shared.entityList.updatedAgo', 'Updated {{distance}} ago', { distance: formatDistanceToNow(parsed) });
}
