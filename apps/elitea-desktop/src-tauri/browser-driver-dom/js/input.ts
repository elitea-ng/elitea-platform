// The crate's build.rs compiles this file with the web app's pinned
// TypeScript into $OUT_DIR/input.js, and src/lib.rs embeds it as
// INPUT_SCRIPT. No JavaScript is committed.
//
// The person's input reporter for the Windows pane (ADR-0048 decision 4).
// WebView2 has no WebKit message handlers, so the host installs this script
// over CDP in an isolated world of its own (`Page.addScriptToEvaluateOnNewDocument`
// with a `worldName`) and exposes a binding in that world only
// (`Runtime.addBinding` with an `executionContextName`); each call of the
// binding is a `Runtime.bindingCalled` event on the host. The page's own
// scripts live in the page world: they cannot see the binding or call it.
//
// Only trusted events count (the page's scripts cannot make one), listened
// for in the capture phase of this world, at most one report every 50 ms
// per event type: the same list and rule as driver.ts.
'use strict';

declare var eliteaBrowserInput: ((type: string) => void) | undefined;

(function reportInput() {
  const MARK = '__eliteaBrowserInputReporter';
  if (Object.prototype.hasOwnProperty.call(globalThis, MARK)) return;
  Object.defineProperty(globalThis, MARK, { value: true });
  const binding = eliteaBrowserInput;
  if (typeof binding !== 'function') return;
  const last = new Map<string, number>();
  const report = (event: Event): void => {
    if (!event.isTrusted) return;
    const now = Date.now();
    if (now - (last.get(event.type) ?? 0) < 50) return;
    last.set(event.type, now);
    try {
      binding(event.type);
    } catch (_) {
      // The binding is gone (the page is closing).
    }
  };
  // The same list as the host's `input::REPORTED`.
  for (const type of ['pointerdown', 'keydown', 'wheel', 'touchstart', 'beforeinput', 'paste', 'drop']) {
    window.addEventListener(type, report, { capture: true, passive: true });
  }
})();
