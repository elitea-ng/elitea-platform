/**
 * The web push service worker's entry: bundled by the `elitea-push-worker`
 * Vite plugin into the classic script `push-sw.js` at the app's base path.
 */
import { installPushWorker, type PushWorkerScope } from './pushWorker';

// The worker's global is a ServiceWorkerGlobalScope; the app's tsconfig types
// `self` as a Window (DOM lib), so it is narrowed to the part the worker uses.
installPushWorker(self as unknown as PushWorkerScope);
