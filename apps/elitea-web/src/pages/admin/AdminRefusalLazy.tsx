import { lazy } from 'react';

// The whole refusal path (StatusPage, the three pages, their helpers) sits
// behind ONE dynamic import: a granted operator loads none of it, which keeps
// it out of the admin initial-bundle budget.
export const AdminRefusalLazy = lazy(() => import('./AdminRefusal').then((m) => ({ default: m.AdminRefusal })));
