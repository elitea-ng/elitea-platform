/**
 * The five callout components of the MDX contract (PREAMBLE §3): `Note`,
 * `Tip`, `Info`, `Warning`, `Check`. Same shape, different colour/icon —
 * built on one internal `Callout` so the five stay visually consistent, but
 * exported as five distinct named components because MDX resolves a JSX tag
 * to a component map key by that exact name (`<Tip>` looks up `Tip`, not
 * `Callout` with a prop).
 */
import type { ReactNode } from 'react';
import { AlertTriangle, CheckCircle2, Info as InfoIcon, Lightbulb, Pin } from 'lucide-react';

import { t } from '@/shared/i18n';

import { Icon } from './Icon';

interface CalloutProps {
  /** Replaces the kind's default label ("Note", "Warning", …) as the heading. */
  readonly title?: string;
  readonly children?: ReactNode;
}

function Callout({
  kind,
  label,
  icon,
  children,
}: {
  readonly kind: string;
  readonly label: string;
  readonly icon: ReactNode;
  readonly children?: ReactNode;
}) {
  return (
    <div className={`docs-callout docs-callout--${kind}`} role="note">
      <div className="docs-callout__icon" aria-hidden="true">
        {icon}
      </div>
      <div className="docs-callout__body">
        <p className="docs-callout__label">{label}</p>
        {children}
      </div>
    </div>
  );
}

export function Note({ title, children }: CalloutProps) {
  return (
    <Callout kind="note" label={title ?? t('entries.docs.callout.note', 'Note')} icon={<Icon as={Pin} />}>
      {children}
    </Callout>
  );
}

export function Tip({ title, children }: CalloutProps) {
  return (
    <Callout kind="tip" label={title ?? t('entries.docs.callout.tip', 'Tip')} icon={<Icon as={Lightbulb} />}>
      {children}
    </Callout>
  );
}

export function Info({ title, children }: CalloutProps) {
  return (
    <Callout kind="info" label={title ?? t('entries.docs.callout.info', 'Info')} icon={<Icon as={InfoIcon} />}>
      {children}
    </Callout>
  );
}

export function Warning({ title, children }: CalloutProps) {
  return (
    <Callout kind="warning" label={title ?? t('entries.docs.callout.warning', 'Warning')} icon={<Icon as={AlertTriangle} />}>
      {children}
    </Callout>
  );
}

export function Check({ title, children }: CalloutProps) {
  return (
    <Callout kind="check" label={title ?? t('entries.docs.callout.check', 'Check')} icon={<Icon as={CheckCircle2} />}>
      {children}
    </Callout>
  );
}
