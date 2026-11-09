import type { SupportContact } from '@elitea/client-core/support';

import { ListRow } from '@/ui';

/**
 * "IT support": the support line an administrator set (ADR-0052 W6.2), as written. It opens
 * only when the line has an email address, a phone number or an https address; otherwise it
 * is text to read. Nothing at all without a line.
 */
export function SupportContactRow({ contact, onPress }: { contact: SupportContact | null; onPress: (() => void) | null }) {
  if (contact === null) return null;
  const opens = contact.href !== null && onPress !== null;
  return (
    <ListRow
      icon={contact.href?.startsWith('tel:') ? 'phone' : contact.href?.startsWith('mailto:') ? 'mention' : 'people'}
      title="IT support"
      subtitle={contact.text}
      accessibilityHint={opens ? 'Contact your IT support' : undefined}
      onPress={opens ? onPress : undefined}
    />
  );
}
