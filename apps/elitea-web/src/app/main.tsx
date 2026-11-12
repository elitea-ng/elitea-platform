// Before any schema-defining module: see zodJitless.ts.
import '@/shared/config/zodJitless';

import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { applyStartLocale } from './applyStartLocale';

const container = document.getElementById('root');
if (!container) {
  throw new Error('elitea-web: #root container missing from index.html');
}

// The saved language is applied before `App` (and so every module that calls
// `t()` at its top level) is imported. Without this the Language choice in
// the account menu saved a value nothing on the web ever read, and the UI
// stayed in English.
void applyStartLocale()
  .catch(() => undefined)
  .then(() => import('./App'))
  .then(({ App }) => {
    createRoot(container).render(
      <StrictMode>
        <App />
      </StrictMode>,
    );
  });
