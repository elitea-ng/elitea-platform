/**
 * The IT support line (ADR-0052 decision 7, client contract 1.45): one line
 * of text an administrator sets, shown in About and Settings of the mobile and
 * desktop apps. Two sources: the device's managed configuration
 * (`support_contact`, set by the MDM) and the workspace policy
 * (`client_policy.support_contact`). The managed one wins.
 *
 * The text is shown as written. At most one link is made from it, and only
 * of three kinds: an email address (`mailto:`), a phone number (`tel:`) or an
 * `https` address. Anything else is plain text, never a link: the line comes
 * from configuration, not from the person, but a `javascript:` or `file:`
 * address in it must still not open.
 */

/** The server's bound (`platformconfig.MaxSupportContactRunes`), in characters. */
export const MAX_SUPPORT_CONTACT_CHARS = 200;

export interface SupportContact {
  /** The line as shown. */
  readonly text: string;
  /** `mailto:`, `tel:` or `https:` address to open, or null: text only. */
  readonly href: string | null;
}

// Control characters (C0, DEL, C1) and the bidirectional overrides and isolates, which could
// make the line read differently from what it is.
// eslint-disable-next-line no-control-regex
const UNSAFE = /[\u0000-\u001f\u007f-\u009f‪-‮⁦-⁩]/u;

/** A configured value as the apps use it: trimmed, one line, bounded; anything else is none. */
export function normalizeSupportContact(raw: unknown): string | null {
  if (typeof raw !== 'string') return null;
  const text = raw.trim();
  if (text === '' || UNSAFE.test(text) || [...text].length > MAX_SUPPORT_CONTACT_CHARS) return null;
  return text;
}

/** Managed beats workspace; each is normalized first, so an invalid managed value does not hide the workspace's. */
export function effectiveSupportContact(managed: unknown, workspace: unknown): string | null {
  return normalizeSupportContact(managed) ?? normalizeSupportContact(workspace);
}

const EMAIL = /[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+/;
const HTTPS = /https:\/\/[^\s<>"'`]+/i;
// A phone number: an optional +, then digits with spaces, dots, dashes or brackets, 7 to 15 digits.
const PHONE = /\+?\(?\d[\d\s().-]{5,}\d/;

/** The line and the one link made from it: the first https address, else email, else phone number. */
export function supportContactLink(text: string): SupportContact {
  const https = HTTPS.exec(text)?.[0]?.replace(/[.,;:!?)\]]+$/, '');
  if (https !== undefined) {
    // An https address that is not a plain one (credentials, no host) makes no link at all:
    // its userinfo must not turn into an email link either.
    return { text, href: plainHttps(https) };
  }
  const email = EMAIL.exec(text)?.[0];
  if (email !== undefined) return { text, href: `mailto:${email}` };
  const phone = PHONE.exec(text)?.[0];
  if (phone !== undefined) {
    const digits = phone.replace(/[^\d]/g, '');
    if (digits.length >= 7 && digits.length <= 15) return { text, href: `tel:${phone.trim().startsWith('+') ? '+' : ''}${digits}` };
  }
  return { text, href: null };
}

function plainHttps(raw: string): string | null {
  try {
    const url = new URL(raw);
    return url.protocol === 'https:' && url.username === '' && url.password === '' && url.hostname !== '' ? url.href : null;
  } catch {
    return null;
  }
}
