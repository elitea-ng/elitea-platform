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
  })
  .catch((error: unknown) => {
    // The app chunk did not load (a stale deploy, a network blip). With the
    // static import this failed loudly; a dynamic one would leave a blank
    // page, so say so and let a reload fetch the current build.
    console.error('elitea-web: the application failed to load', error);
    container.textContent = 'The application failed to load. Reload the page to try again.';
  });
