/**
 * The admin shell's refusal path, as ONE module so `AdminApp` can load it with a
 * single dynamic import. A granted operator never fetches any of this.
 */
import type { ReactNode } from 'react';

import { AdminAccessDenied } from './AdminAccessDenied';
import { AdminAccessUnavailable } from './AdminAccessUnavailable';
import { AdminSignInRedirect } from './AdminSignInRedirect';

export type AdminRefusalGate = 'denied' | 'unauthenticated' | 'unavailable';

export function AdminRefusal({ gate }: { readonly gate: AdminRefusalGate }): ReactNode {
  if (gate === 'unauthenticated') return <AdminSignInRedirect />;
  if (gate === 'unavailable') return <AdminAccessUnavailable />;
  return <AdminAccessDenied />;
}
