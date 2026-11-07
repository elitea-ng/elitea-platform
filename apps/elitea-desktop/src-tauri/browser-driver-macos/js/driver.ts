// The crate's build.rs compiles this file with the web app's pinned
// TypeScript (apps/elitea-web/node_modules/.bin/tsc, or ELITEA_TSC) into
// $OUT_DIR/driver.js, and src/lib.rs embeds that output as SCRIPT. This is
// the only script file in the repository: no JavaScript is committed, and a
// type error here fails the cargo build.
//
// The macOS browser driver's page script (ADR-0048 decision 6).
//
// It runs ONLY in the driver's private WKContentWorld: installed as a user
// script in that world (every frame, at document start) and called with
// `callAsyncJavaScript(_:arguments:in:in:)` in that world. The page's own
// scripts live in the page world: they cannot see these globals, call these
// functions, or replace the prototypes used here (each world has its own
// JavaScript globals over the same DOM). The model never supplies any of
// this code: the host sends an operation name and JSON arguments only.
//
// Nothing here returns the value of a secure field (a password field, or a
// field whose autocomplete names a password or a card number or security
// code); the host decides "secure" again from `t` and `ac` with the
// contract's `is_secure_field`, and the snapshot drops any value it has.
'use strict';

// ---- types (erased on emit) -----------------------------------------------
//
// The answers below are the shapes `src/reply.rs` reads: `RawNode`,
// `RawPage`, `RawState`, `RawClick` and `RawRect`, inside `{"ok": …}` or
// `{"error": {"code", "message"}}`.

/** The error codes the script gives (`reply::SCRIPT_CODES`, plus `internal`). */
type ScriptCode = 'stale_ref' | 'secure_field_refused' | 'invalid_argument' | 'internal';

/** An error made by `failure`: its code is one the host knows. */
type DriverError = Error & { code?: ScriptCode };

/** A text run of the walk (`RawNode` with `k` 0). */
interface RawText {
  r: 'text';
  n: string;
}

/** An element of the walk (`RawNode`). */
interface RawElement {
  /** Role. */
  r: string;
  /** Accessible name. */
  n: string;
  /** The element's number in its frame's document. */
  k: number;
  /** State flags as the grammar spells them (`checked`, `level=2`). */
  s: string[];
  c: RawNode[];
  /** `type` and `autocomplete` of an `input` or `textarea`. */
  t?: string;
  ac?: string;
  /** Value (never sent for a secure field). */
  v?: string;
  /** Actionable although its role is not interactive. */
  a?: boolean;
  /** A cross-origin frame's address: its content is read in that frame. */
  x?: string;
}

type RawNode = RawText | RawElement;

/** A frame's walk (`RawPage`). */
interface RawPage {
  doc: string;
  url: string;
  title: string;
  nodes: RawNode[];
  /** The `k` of every cross-origin frame element, in document order. */
  frames: number[];
}

/** A frame's document and where it is (`RawState`). */
interface RawState {
  doc: string;
  url: string;
  title: string;
  ready: DocumentReadyState;
}

/** A rectangle in the top viewport's CSS pixels (`RawRect`). */
interface RawRect {
  x: number;
  y: number;
  width: number;
  height: number;
}

/** A click's answer (`RawClick`). */
interface RawClick {
  ignored: boolean;
  rect: RawRect | null;
}

/** Text mode's answer. */
interface PageText {
  url: string;
  title: string;
  text: string;
}

/** An action's answer when there is nothing to tell. */
type Done = Record<string, never>;

/** `press` when a cross-origin frame has the focus. */
interface InFrame {
  in_frame: true;
}

/** An element as the host's rules read it (`RawControl`): `focused_control`
 * and `inspect`. */
interface RawControl {
  /** Role (empty when it has none). */
  r: string;
  /** Accessible name. */
  n: string;
  /** An `input`'s `type`, lowercase. */
  t?: string;
  /** A link with a `download` attribute. */
  d: boolean;
}

type Ok = RawState | RawPage | PageText | RawRect | RawClick | RawControl | Done | InFrame | boolean | number | null;

interface OkAnswer {
  ok: Ok;
}

/** A `failure`'s code and message, or whatever a thrown value carried
 * (the host keeps only the codes it knows and its own message for the rest). */
interface ErrorAnswer {
  error: { code: unknown; message: unknown };
}

type Answer = OkAnswer | ErrorAnswer;

/** What the driver world exposes, under `__eliteaBrowserDriver`. */
interface DriverApi {
  readonly run: (op: string, json: string) => Promise<string>;
}

/** The elements the script narrows to by tag name. */
interface Tags {
  A: HTMLAnchorElement;
  BUTTON: HTMLButtonElement;
  DETAILS: HTMLDetailsElement;
  FRAME: HTMLFrameElement;
  IFRAME: HTMLIFrameElement;
  INPUT: HTMLInputElement;
  METER: HTMLMeterElement;
  OPTION: HTMLOptionElement;
  OUTPUT: HTMLOutputElement;
  PROGRESS: HTMLProgressElement;
  SELECT: HTMLSelectElement;
  SLOT: HTMLSlotElement;
  TABLE: HTMLTableElement;
  TEXTAREA: HTMLTextAreaElement;
}

// The driver world's global (defined once, by `install`).
declare var __eliteaBrowserDriver: DriverApi | undefined;

// The message handlers the host registers in the driver world. A host
// without the handler (the lazy install of a call) has none: posting then
// throws, and the announcement catches that.
interface Window {
  readonly webkit: {
    readonly messageHandlers: {
      readonly eliteaBrowserFrame: { postMessage(message: string): void };
      readonly eliteaBrowserInput: { postMessage(message: string): void };
    };
  };
}

(function install() {
  const KEY = '__eliteaBrowserDriver';
  if (globalThis[KEY]) return;

  // Captured at install, before any page script can run in this frame;
  // the page world could not reach them anyway.
  const getComputedStyleOf = (el: Element): CSSStyleDeclaration => viewOf(el).getComputedStyle(el);
  const inputValue = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value');
  const textareaValue = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value');
  const stringify = JSON.stringify;
  const parse: (text: string) => unknown = JSON.parse;

  const MAX_RAW_NODES = 20000;
  const MAX_TEXT = 2000;
  const SECURE_TOKENS = ['current-password', 'new-password', 'cc-number', 'cc-csc'];
  const SKIPPED = new Set(['SCRIPT', 'STYLE', 'NOSCRIPT', 'TEMPLATE', 'HEAD', 'META', 'LINK', 'TITLE', 'BASE']);
  // Roles whose children are presentational (WAI-ARIA 1.2): the snapshot
  // shows the control, not its insides.
  const LEAF_ROLES = new Set(['button', 'checkbox', 'img', 'meter', 'menuitemcheckbox', 'menuitemradio', 'progressbar', 'radio', 'scrollbar', 'separator', 'slider', 'spinbutton', 'switch', 'tab', 'textbox', 'searchbox']);
  // Roles named from their content when nothing else names them.
  const NAME_FROM_CONTENT = new Set(['button', 'cell', 'checkbox', 'columnheader', 'gridcell', 'heading', 'link', 'menuitem', 'menuitemcheckbox', 'menuitemradio', 'option', 'radio', 'row', 'rowheader', 'switch', 'tab', 'tooltip', 'treeitem', 'legend', 'caption', 'term', 'definition', 'strong', 'emphasis', 'code']);
  const INTERACTIVE = new Set(['button', 'checkbox', 'combobox', 'link', 'listbox', 'menuitem', 'menuitemcheckbox', 'menuitemradio', 'option', 'radio', 'searchbox', 'slider', 'spinbutton', 'switch', 'tab', 'textbox', 'treeitem']);
  const RANGE_ROLES = new Set(['slider', 'spinbutton', 'progressbar', 'scrollbar', 'meter']);

  // ---- typed access ---------------------------------------------------------
  //
  // The DOM here is dynamic: the tag decides which IDL attributes an element
  // has, a thrown value can be anything, and the host's arguments are JSON.
  // These read it as the plain property access would, with types.

  // `value[name]`, for a value of unknown shape (throws on null and
  // undefined, as the plain access does).
  function field(value: unknown, name: string): unknown {
    if (value === null || value === undefined) throw new TypeError('cannot read a property of nothing');
    return Reflect.get(Object(value), name, value);
  }

  // `el.tagName === tag`, narrowing `el` to that element's interface.
  function is<T extends keyof Tags>(el: Element, tag: T): el is Tags[T] {
    return el.tagName === tag;
  }

  function isElement(node: Node): node is Element {
    return node.nodeType === 1;
  }

  function isText(node: Node): node is Text {
    return node.nodeType === 3;
  }

  // `isContentEditable` and `innerText` are HTMLElement's: other elements
  // (SVG, MathML) have neither, and read as not editable.
  function contentEditable(el: Element): boolean {
    return field(el, 'isContentEditable') === true;
  }

  // Elements with `tabIndex` and `focus()`: HTML, SVG and MathML ones.
  function hasTabIndex(el: Element): el is Element & HTMLOrSVGElement {
    return typeof field(el, 'tabIndex') === 'number';
  }

  function canFocus(el: Element): el is Element & HTMLOrSVGElement {
    return typeof field(el, 'focus') === 'function';
  }

  // `el.click()` for any element: HTMLElement's; on another kind of element
  // it fails as the plain call would.
  function clickOn(el: Element): void {
    const click = field(el, 'click');
    if (typeof click !== 'function') throw new TypeError('the element has no click');
    Reflect.apply(click, el, []);
  }

  // The window of an element's document. A document without one fails as
  // reading through `null` would.
  function viewOf(el: Element): Window & typeof globalThis {
    const view = el.ownerDocument.defaultView;
    if (view === null) throw new TypeError('the element has no window');
    return view;
  }

  // A captured accessor; a missing one fails as calling it would.
  function accessor<F>(f: F | undefined): F {
    if (f === undefined) throw new TypeError('the accessor is missing');
    return f;
  }

  // A JSON list of strings (`list.map(String)`); anything else fails as
  // calling `map` on it would.
  function strings(value: unknown): string[] {
    if (!Array.isArray(value)) throw new TypeError('not a list');
    const list: readonly unknown[] = value;
    return list.map(String);
  }

  // ---- identity -----------------------------------------------------------

  const random = (): string => {
    const bytes = new Uint8Array(12);
    crypto.getRandomValues(bytes);
    return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
  };
  const doc = random();
  const frameToken = random();
  // Keyed by the numbers `idFor` hands out; the host's ids are looked up as sent.
  const elements = new Map<unknown, Element>();
  const ids = new WeakMap<Element, number>();
  let nextId = 1;

  function idFor(el: Element): number {
    let id = ids.get(el);
    if (id === undefined) {
      id = nextId++;
      ids.set(el, id);
    }
    elements.set(id, el);
    return id;
  }

  function byId(id: unknown): Element {
    const el = elements.get(id);
    if (!el || !el.isConnected) {
      elements.delete(id);
      throw failure('stale_ref', 'that element is gone from the page: snapshot again');
    }
    return el;
  }

  function failure(code: ScriptCode, message: string): DriverError {
    const error: DriverError = new Error(message);
    error.code = code;
    return error;
  }

  // ---- roles ----------------------------------------------------------------

  function inputRole(el: Element): string | null {
    const type = (el.getAttribute('type') || 'text').toLowerCase();
    switch (type) {
      case 'checkbox': return 'checkbox';
      case 'radio': return 'radio';
      case 'range': return 'slider';
      case 'number': return 'spinbutton';
      case 'search': return el.hasAttribute('list') ? 'combobox' : 'searchbox';
      case 'button': case 'submit': case 'reset': case 'image': return 'button';
      case 'hidden': return null;
      case 'color': case 'file': case 'date': case 'datetime-local': case 'month': case 'time': case 'week': return 'textbox';
      default: return el.hasAttribute('list') ? 'combobox' : 'textbox';
    }
  }

  function inLandmarkScope(el: Element): boolean {
    for (let up = el.parentElement; up; up = up.parentElement) {
      if (['ARTICLE', 'ASIDE', 'MAIN', 'NAV', 'SECTION'].includes(up.tagName)) return true;
    }
    return false;
  }

  function implicitRole(el: Element): string | null {
    switch (el.tagName) {
      case 'A': case 'AREA': return el.hasAttribute('href') ? 'link' : 'generic';
      case 'ARTICLE': return 'article';
      case 'ASIDE': return 'complementary';
      case 'BLOCKQUOTE': return 'blockquote';
      case 'BUTTON': return 'button';
      case 'CODE': return 'code';
      case 'DATALIST': return 'listbox';
      case 'DD': return 'definition';
      case 'DETAILS': return 'group';
      case 'DIALOG': return 'dialog';
      case 'DT': return 'term';
      case 'EM': return 'emphasis';
      case 'FIELDSET': return 'group';
      case 'FIGURE': return 'figure';
      case 'FOOTER': return inLandmarkScope(el) ? 'generic' : 'contentinfo';
      case 'FORM': return hasOwnName(el) ? 'form' : 'generic';
      case 'H1': case 'H2': case 'H3': case 'H4': case 'H5': case 'H6': return 'heading';
      case 'HEADER': return inLandmarkScope(el) ? 'generic' : 'banner';
      case 'HR': return 'separator';
      case 'IFRAME': case 'FRAME': return 'iframe';
      case 'IMG': return el.getAttribute('alt') === '' ? 'presentation' : 'img';
      case 'INPUT': return inputRole(el);
      case 'LI': return 'listitem';
      case 'MAIN': return 'main';
      case 'MATH': return 'math';
      case 'MENU': case 'OL': case 'UL': return 'list';
      case 'NAV': return 'navigation';
      case 'OPTGROUP': return 'group';
      case 'OPTION': return 'option';
      case 'OUTPUT': return 'status';
      case 'P': return 'paragraph';
      case 'PROGRESS': return 'progressbar';
      case 'SEARCH': return 'search';
      case 'SECTION': return hasOwnName(el) ? 'region' : 'generic';
      case 'SELECT': return is(el, 'SELECT') && (el.multiple || el.size > 1) ? 'listbox' : 'combobox';
      case 'STRONG': return 'strong';
      case 'SUMMARY': return 'button';
      case 'TABLE': return 'table';
      case 'TBODY': case 'THEAD': case 'TFOOT': return 'rowgroup';
      case 'TD': return 'cell';
      case 'TH': return el.getAttribute('scope') === 'row' ? 'rowheader' : 'columnheader';
      case 'TEXTAREA': return 'textbox';
      case 'TR': return 'row';
      case 'METER': return 'meter';
      default: return contentEditable(el) && el.parentElement && !contentEditable(el.parentElement) ? 'textbox' : 'generic';
    }
  }

  function roleOf(el: Element): string | null {
    const explicit = (el.getAttribute('role') || '').trim().toLowerCase().split(/\s+/)[0];
    if (explicit && !((explicit === 'none' || explicit === 'presentation') && focusable(el))) return explicit;
    return implicitRole(el);
  }

  function focusable(el: Element): boolean {
    if (el.hasAttribute('tabindex')) return true;
    return ['A', 'BUTTON', 'INPUT', 'SELECT', 'TEXTAREA'].includes(el.tagName) && !field(el, 'disabled');
  }

  // ---- names (a subset of accname 1.2) ------------------------------------

  function hasOwnName(el: Element): boolean {
    return (el.getAttribute('aria-label') || '').trim() !== '' || (el.getAttribute('aria-labelledby') || '').trim() !== '' || (el.getAttribute('title') || '').trim() !== '';
  }

  function rendered(el: Element): boolean {
    if (el.getAttribute('aria-hidden') === 'true') return false;
    const style = getComputedStyleOf(el);
    return style.display !== 'none' && style.visibility !== 'hidden' && style.visibility !== 'collapse';
  }

  function childNodesOf(node: Node): Node[] {
    if (isElement(node)) {
      if (node.shadowRoot) return Array.from(node.shadowRoot.childNodes);
      if (is(node, 'SLOT')) {
        const assigned = node.assignedNodes({ flatten: true });
        return assigned.length > 0 ? assigned : Array.from(node.childNodes);
      }
    }
    return Array.from(node.childNodes);
  }

  // A document or a shadow root: a root with `getElementById`.
  function hasGetElementById(root: Node): root is Node & NonElementParentNode {
    return Boolean(field(root, 'getElementById'));
  }

  function byIdRef(el: Element, id: string): Element | null {
    const root = el.getRootNode();
    return (hasGetElementById(root) ? root.getElementById(id) : null) || el.ownerDocument.getElementById(id);
  }

  // The text a subtree contributes to a name.
  function textOf(node: Node, skip: Node | null, labelledBy: boolean): string {
    if (isText(node)) return node.data;
    if (!isElement(node) || SKIPPED.has(node.tagName) || node === skip) return '';
    const el = node;
    if (!labelledBy && !rendered(el)) return '';
    const label = (el.getAttribute('aria-label') || '').trim();
    if (label) return label;
    if (is(el, 'INPUT')) {
      const type = (el.getAttribute('type') || 'text').toLowerCase();
      if (['button', 'submit', 'reset'].includes(type)) return el.value;
      if (type === 'image') return el.getAttribute('alt') || '';
      if (isSecureElement(el)) return '';
      return ['checkbox', 'radio', 'hidden', 'file'].includes(type) ? '' : el.value;
    }
    if (is(el, 'TEXTAREA')) return isSecureElement(el) ? '' : el.value;
    if (is(el, 'SELECT')) return Array.from(el.selectedOptions, (o) => o.text).join(' ');
    if (el.tagName === 'IMG' || el.tagName === 'AREA') return el.getAttribute('alt') || '';
    if (el.tagName === 'BR') return ' ';
    let text = '';
    for (const child of childNodesOf(el)) text += textOf(child, skip, labelledBy);
    const display = getComputedStyleOf(el).display;
    return display === 'inline' || display === 'contents' ? text : ` ${text} `;
  }

  // The labelable elements whose labels name them.
  function isLabelled(el: Element): el is HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement | HTMLMeterElement | HTMLProgressElement | HTMLOutputElement {
    const tag = el.tagName;
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || tag === 'METER' || tag === 'PROGRESS' || tag === 'OUTPUT';
  }

  function nameOf(el: Element, role: string): string {
    const labelledby = (el.getAttribute('aria-labelledby') || '').trim();
    if (labelledby) {
      const parts = labelledby.split(/\s+/).map((id) => byIdRef(el, id)).filter((ref): ref is Element => Boolean(ref)).map((ref) => textOf(ref, null, true));
      const text = parts.join(' ').trim();
      if (text) return text;
    }
    const label = (el.getAttribute('aria-label') || '').trim();
    if (label) return label;
    const tag = el.tagName;
    if (isLabelled(el)) {
      const type = (el.getAttribute('type') || '').toLowerCase();
      if (is(el, 'INPUT') && ['button', 'submit', 'reset'].includes(type)) {
        return el.value || (type === 'submit' ? 'Submit' : type === 'reset' ? 'Reset' : '');
      }
      if (tag === 'INPUT' && type === 'image') return el.getAttribute('alt') || 'Submit';
      const labels = el.labels ? Array.from(el.labels) : [];
      const text = labels.map((l) => textOf(l, el, false)).join(' ').trim();
      if (text) return text;
    }
    if (tag === 'IMG' || tag === 'AREA') {
      const alt = el.getAttribute('alt');
      if (alt) return alt;
    }
    if (tag === 'FIELDSET') {
      const legend = Array.from(el.children).find((c) => c.tagName === 'LEGEND');
      if (legend) return textOf(legend, null, false);
    }
    if (is(el, 'TABLE') && el.caption) return textOf(el.caption, null, false);
    if (tag === 'FIGURE') {
      const caption = Array.from(el.children).find((c) => c.tagName === 'FIGCAPTION');
      if (caption) return textOf(caption, null, false);
    }
    if (NAME_FROM_CONTENT.has(role)) {
      let text = '';
      for (const child of childNodesOf(el)) text += textOf(child, null, false);
      if (text.trim()) return text;
    }
    const title = el.getAttribute('title') || '';
    if (title.trim()) return title;
    if ((is(el, 'INPUT') || is(el, 'TEXTAREA')) && el.placeholder) return el.placeholder;
    return '';
  }

  // ---- values and states ---------------------------------------------------

  function isSecureElement(el: Element): boolean {
    const type = (el.getAttribute('type') || '').trim().toLowerCase();
    const tokens = (el.getAttribute('autocomplete') || '').toLowerCase().split(/\s+/);
    return type === 'password' || tokens.some((token) => SECURE_TOKENS.includes(token));
  }

  function valueOf(el: Element, role: string): string | undefined {
    if (isSecureElement(el)) return undefined;
    if (is(el, 'SELECT')) return Array.from(el.selectedOptions, (o) => o.text).join(', ');
    if (RANGE_ROLES.has(role)) {
      const text = el.getAttribute('aria-valuetext') || el.getAttribute('aria-valuenow');
      if (text !== null) return text;
      if (is(el, 'INPUT') || is(el, 'METER') || is(el, 'PROGRESS')) return String(el.value);
      return undefined;
    }
    if (is(el, 'INPUT')) {
      const type = (el.getAttribute('type') || 'text').toLowerCase();
      if (['checkbox', 'radio', 'button', 'submit', 'reset', 'image', 'file', 'hidden'].includes(type)) return undefined;
      return el.value;
    }
    if (is(el, 'TEXTAREA')) return el.value;
    if ((role === 'textbox' || role === 'searchbox') && contentEditable(el)) {
      const text = field(el, 'innerText');
      return typeof text === 'string' ? text : undefined;
    }
    return undefined;
  }

  function deepActive(document: Document): Element | null {
    let active = document.activeElement;
    while (active && active.shadowRoot && active.shadowRoot.activeElement) active = active.shadowRoot.activeElement;
    while (active && is(active, 'IFRAME')) {
      try {
        const inner: Element | null = active.contentDocument && active.contentDocument.activeElement;
        if (!inner) break;
        active = inner;
      } catch (_) {
        break;
      }
    }
    return active;
  }

  function statesOf(el: Element, role: string): string[] {
    const states: string[] = [];
    const aria = (name: string): string | null => el.getAttribute(name);
    if (role === 'heading') {
      const level = Number(aria('aria-level')) || (/^H[1-6]$/.test(el.tagName) ? Number(el.tagName[1]) : 2);
      states.push(`level=${level}`);
    }
    if (is(el, 'INPUT') && (el.type === 'checkbox' || el.type === 'radio')) {
      if (el.indeterminate) states.push('checked=mixed');
      else if (el.checked) states.push('checked');
    } else if (aria('aria-checked') === 'true') states.push('checked');
    else if (aria('aria-checked') === 'mixed') states.push('checked=mixed');
    if (aria('aria-pressed') === 'true') states.push('pressed');
    else if (aria('aria-pressed') === 'mixed') states.push('pressed=mixed');
    if (is(el, 'OPTION') ? el.selected : aria('aria-selected') === 'true') states.push('selected');
    if (aria('aria-expanded') === 'true') states.push('expanded');
    else if (aria('aria-expanded') === 'false') states.push('collapsed');
    else if (el.tagName === 'SUMMARY' && el.parentElement && is(el.parentElement, 'DETAILS')) states.push(el.parentElement.open ? 'expanded' : 'collapsed');
    if (aria('aria-disabled') === 'true' || (typeof el.matches === 'function' && el.matches(':disabled'))) states.push('disabled');
    if (aria('aria-readonly') === 'true' || ((is(el, 'INPUT') || is(el, 'TEXTAREA')) && el.readOnly)) states.push('readonly');
    if (aria('aria-required') === 'true' || field(el, 'required') === true) states.push('required');
    const invalid = aria('aria-invalid');
    if (invalid && invalid !== 'false') states.push('invalid');
    if (el === deepActive(el.ownerDocument) && el !== el.ownerDocument.body) states.push('focused');
    return states;
  }

  // ---- the walk -------------------------------------------------------------

  function snapshot(maxRaw: unknown): RawPage {
    let budget = Math.min(Number(maxRaw || MAX_RAW_NODES), MAX_RAW_NODES);
    const frames: number[] = [];

    function walkChildren(parent: Node, out: RawNode[]): void {
      for (const child of childNodesOf(parent)) {
        if (budget <= 0) return;
        if (isText(child)) {
          if (/\S/.test(child.data)) {
            const host = child.parentElement;
            if (host && getComputedStyleOf(host).visibility !== 'visible') continue;
            budget--;
            out.push({ r: 'text', n: child.data.slice(0, MAX_TEXT) });
          }
        } else if (isElement(child)) {
          walkElement(child, out);
        }
      }
    }

    function walkElement(el: Element, out: RawNode[]): void {
      if (SKIPPED.has(el.tagName) || el.getAttribute('aria-hidden') === 'true') return;
      const style = getComputedStyleOf(el);
      if (style.display === 'none' || style.contentVisibility === 'hidden') return;
      if (style.visibility === 'hidden' || style.visibility === 'collapse') {
        walkChildren(el, out);
        return;
      }
      const role = roleOf(el);
      if (role === null) return;
      budget--;
      const node: RawElement = { r: role, n: nameOf(el, role).slice(0, MAX_TEXT), k: idFor(el), s: statesOf(el, role), c: [] };
      if (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA') {
        node.t = el.getAttribute('type') || '';
        node.ac = el.getAttribute('autocomplete') || '';
      }
      const value = valueOf(el, role);
      if (value !== undefined && value !== '') node.v = String(value).slice(0, MAX_TEXT);
      if (!INTERACTIVE.has(role) && (el.hasAttribute('onclick') || (el.hasAttribute('tabindex') && hasTabIndex(el) && el.tabIndex >= 0))) node.a = true;
      if (is(el, 'IFRAME') || is(el, 'FRAME')) {
        let inner: Document | null = null;
        try {
          inner = el.contentDocument;
        } catch (_) {
          inner = null;
        }
        if (inner && inner.body) {
          walkChildren(inner.body, node.c);
        } else {
          // Cross-origin: the host snapshots that frame in this world
          // and puts its nodes here.
          node.x = el.src || '';
          frames.push(node.k);
        }
      } else if (!LEAF_ROLES.has(role) && el.tagName !== 'SELECT') {
        walkChildren(el, node.c);
      } else if (is(el, 'SELECT')) {
        for (const option of el.querySelectorAll('option')) {
          budget--;
          const name = (option.label || option.text || '').slice(0, MAX_TEXT);
          node.c.push({ r: 'option', n: name, k: idFor(option), s: statesOf(option, 'option'), c: [] });
        }
      }
      out.push(node);
    }

    const nodes: RawNode[] = [];
    if (document.body) walkChildren(document.body, nodes);
    return { doc, url: location.href, title: document.title, nodes, frames };
  }

  // ---- text ----------------------------------------------------------------

  function pageText(max: unknown): PageText {
    const parts: string[] = [];
    const add = (d: Document | null): void => {
      if (!d || !d.body) return;
      parts.push(d.body.innerText);
      for (const frame of d.querySelectorAll<HTMLIFrameElement | HTMLFrameElement>('iframe, frame')) {
        try {
          add(frame.contentDocument);
        } catch (_) {
          // Cross-origin frames are left out of text mode.
        }
      }
    };
    add(document);
    return { url: location.href, title: document.title, text: parts.join('\n').slice(0, Number(max || 100000)) };
  }

  function hasText(text: string): boolean {
    return pageText(1000000).text.replace(/\s+/g, ' ').includes(text.replace(/\s+/g, ' ').trim());
  }

  // ---- geometry ------------------------------------------------------------

  // The element's rectangle in this frame's TOP-LEVEL viewport (the same
  // origin chain only), in CSS pixels.
  function rectOf(el: Element): RawRect {
    const r = el.getBoundingClientRect();
    let x = r.left;
    let y = r.top;
    let view: Window | null = el.ownerDocument.defaultView;
    while (view && view.frameElement) {
      const f = view.frameElement.getBoundingClientRect();
      x += f.left + view.frameElement.clientLeft;
      y += f.top + view.frameElement.clientTop;
      view = view.parent;
    }
    return { x, y, width: r.width, height: r.height };
  }

  // ---- actions --------------------------------------------------------------

  function center(el: Element): { clientX: number; clientY: number } {
    const r = el.getBoundingClientRect();
    return { clientX: r.left + r.width / 2, clientY: r.top + r.height / 2 };
  }

  function mouse(el: Element, type: string, extra?: MouseEventInit): boolean {
    const view = viewOf(el);
    const Ctor = type.startsWith('pointer') ? view.PointerEvent || view.MouseEvent : view.MouseEvent;
    const init: PointerEventInit = { bubbles: true, cancelable: true, composed: true, view, button: 0, buttons: type.endsWith('down') ? 1 : 0, detail: 1, ...center(el), ...extra };
    if (type.startsWith('pointer')) Object.assign(init, { pointerId: 1, pointerType: 'mouse', isPrimary: true });
    return el.dispatchEvent(new Ctor(type, init));
  }

  // What the page shows of itself, to tell whether a click did anything.
  function observe(el: Element): () => boolean {
    const d = el.ownerDocument;
    const before = { url: location.href, active: d.activeElement, state: controlState(el) };
    let mutated = false;
    const watcher = new MutationObserver(() => {
      mutated = true;
    });
    watcher.observe(document, { subtree: true, childList: true, attributes: true, characterData: true });
    return () => {
      watcher.disconnect();
      return mutated || location.href !== before.url || d.activeElement !== before.active || controlState(el) !== before.state;
    };
  }

  function controlState(el: Element): string {
    return `${field(el, 'checked')}|${field(el, 'value')}|${field(el, 'selected')}|${field(el, 'open')}`;
  }

  const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

  async function click(id: unknown, double: boolean): Promise<RawClick> {
    const el = byId(id);
    el.scrollIntoView({ block: 'center', inline: 'center' });
    // The focus moves as a real press would move it; that is the driver's
    // doing, not the page answering, so it is taken before observing.
    if (canFocus(el)) el.focus({ preventScroll: true });
    const changed = observe(el);
    // Every event is sent even when an earlier one was cancelled (`&`, not
    // `&&`), as a real press sends them all.
    let pressed = Number(mouse(el, 'pointerdown')) & Number(mouse(el, 'mousedown'));
    pressed = pressed & Number(mouse(el, 'pointerup')) & Number(mouse(el, 'mouseup'));
    // A synthetic `click` runs the element's activation behaviour (a link,
    // a submit button, a checkbox) like a real one.
    const accepted = Boolean(pressed & Number(mouse(el, 'click')));
    if (double) {
      mouse(el, 'click', { detail: 2 });
      mouse(el, 'dblclick', { detail: 2 });
    }
    // The page refused the scripted events and nothing changed: it may
    // only accept trusted input (the host's fallback, or the person).
    await sleep(accepted ? 0 : 250);
    const ignored = !accepted && !changed();
    return { ignored, rect: ignored ? rectOf(el) : null };
  }

  function setValue(el: Element, value: string): void {
    if (el.tagName === 'INPUT') accessor(inputValue?.set).call(el, value);
    else if (el.tagName === 'TEXTAREA') accessor(textareaValue?.set).call(el, value);
  }

  function getValue(el: Element): string {
    let value: unknown = '';
    if (el.tagName === 'INPUT') value = accessor(inputValue?.get).call(el);
    else if (el.tagName === 'TEXTAREA') value = accessor(textareaValue?.get).call(el);
    return typeof value === 'string' ? value : '';
  }

  function editable(el: Element | null): el is HTMLElement {
    if (!el) return false;
    if (is(el, 'TEXTAREA')) return !el.readOnly && !el.disabled;
    if (is(el, 'INPUT')) {
      const role = inputRole(el);
      return (role === 'textbox' || role === 'searchbox' || role === 'combobox' || role === 'spinbutton') && !el.readOnly && !el.disabled;
    }
    return contentEditable(el);
  }

  function insertText(el: HTMLElement, text: string): void {
    const view = viewOf(el);
    const allowed = el.dispatchEvent(new view.InputEvent('beforeinput', { bubbles: true, cancelable: true, composed: true, inputType: 'insertText', data: text }));
    if (!allowed) return;
    if (contentEditable(el)) {
      el.ownerDocument.execCommand('insertText', false, text);
    } else {
      setValue(el, getValue(el) + text);
      el.dispatchEvent(new view.InputEvent('input', { bubbles: true, composed: true, inputType: 'insertText', data: text }));
    }
  }

  function submitFrom(el: HTMLInputElement): void {
    const form = el.form || (el.closest && el.closest('form'));
    if (form && typeof form.requestSubmit === 'function') form.requestSubmit();
  }

  function typeText(id: unknown, text: string, clear: boolean, submit: boolean): Done {
    const el = byId(id);
    if (isSecureElement(el)) throw failure('secure_field_refused', 'typing into a password or payment card field is refused');
    if (!editable(el)) throw failure('invalid_argument', 'that element is not a text field');
    el.scrollIntoView({ block: 'center' });
    el.focus({ preventScroll: true });
    const view = viewOf(el);
    if (clear) {
      if (contentEditable(el)) el.textContent = '';
      else setValue(el, '');
      el.dispatchEvent(new view.InputEvent('input', { bubbles: true, composed: true, inputType: 'deleteContentBackward' }));
    }
    insertText(el, text);
    el.dispatchEvent(new view.Event('change', { bubbles: true }));
    if (submit) pressKey('Enter', []);
    return {};
  }

  function selectValues(id: unknown, values: string[]): Done {
    const el = byId(id);
    if (!is(el, 'SELECT')) throw failure('invalid_argument', 'that element is not a select');
    const wanted = new Set(values);
    const matches = Array.from(el.options).filter((o) => wanted.has(o.value) || wanted.has(o.label) || wanted.has(o.text));
    if (matches.length === 0) throw failure('invalid_argument', 'no option has that label or value');
    if (!el.multiple && matches.length > 1) matches.length = 1;
    for (const option of el.options) option.selected = matches.includes(option);
    const view = viewOf(el);
    el.dispatchEvent(new view.Event('input', { bubbles: true, composed: true }));
    el.dispatchEvent(new view.Event('change', { bubbles: true }));
    return {};
  }

  function scrollTo(id: unknown): Done {
    byId(id).scrollIntoView({ block: 'center', inline: 'center' });
    return {};
  }

  function scrollPage(direction: string, pixels: unknown): Done {
    const dy = Number(pixels || Math.round(window.innerHeight * 0.85));
    const dx = Number(pixels || Math.round(window.innerWidth * 0.85));
    const moves: Readonly<Record<string, readonly [number, number] | undefined>> = { up: [0, -dy], down: [0, dy], left: [-dx, 0], right: [dx, 0] };
    const by = moves[direction] || [0, 0];
    window.scrollBy(by[0], by[1]);
    return {};
  }

  function tabbables(d: Document): (Element & HTMLOrSVGElement)[] {
    return Array.from(d.querySelectorAll('a[href], button, input, select, textarea, [tabindex], [contenteditable=""], [contenteditable="true"]')).filter((el): el is Element & HTMLOrSVGElement => hasTabIndex(el) && el.tabIndex >= 0 && !field(el, 'disabled') && rendered(el));
  }

  function pressKey(key: string, mods: readonly string[]): Done | InFrame {
    const target = deepActive(document) || document.body;
    // A cross-origin frame has the focus: the host presses there instead.
    if (target.tagName === 'IFRAME' || target.tagName === 'FRAME') return { in_frame: true };
    if (target !== document.body && isSecureElement(target)) throw failure('secure_field_refused', 'pressing keys into a password or payment card field is refused');
    const has = (m: string): boolean => mods.includes(m);
    const name = key === 'Space' ? ' ' : key;
    const view = viewOf(target);
    const init = { key: name, bubbles: true, cancelable: true, composed: true, ctrlKey: has('Control'), altKey: has('Alt'), shiftKey: has('Shift'), metaKey: has('Meta') };
    const allowed = target.dispatchEvent(new view.KeyboardEvent('keydown', init));
    const printable = name.length === 1 || [...name].length === 1;
    if (allowed) {
      if (printable && !init.ctrlKey && !init.metaKey) {
        target.dispatchEvent(new view.KeyboardEvent('keypress', init));
        if (editable(target)) insertText(target, name);
        else if (name === ' ' && (is(target, 'BUTTON') || (is(target, 'INPUT') && ['checkbox', 'radio', 'button', 'submit'].includes(target.type)))) target.click();
      } else if (name === 'Enter') {
        if (is(target, 'INPUT')) submitFrom(target);
        else if (target.tagName === 'A' || target.tagName === 'BUTTON' || target.getAttribute('role') === 'button') clickOn(target);
      } else if (name === 'Backspace' && editable(target) && !contentEditable(target)) {
        setValue(target, getValue(target).slice(0, -1));
        target.dispatchEvent(new view.InputEvent('input', { bubbles: true, composed: true, inputType: 'deleteContentBackward' }));
      } else if (name === 'Tab') {
        const list = tabbables(target.ownerDocument);
        const at = list.findIndex((el) => el === target);
        const next = list[(at + (init.shiftKey ? -1 : 1) + list.length) % list.length];
        if (next) next.focus();
      }
    }
    target.dispatchEvent(new view.KeyboardEvent('keyup', init));
    return {};
  }

  // ---- the host's rules ----------------------------------------------------
  //
  // What an act would reach, read before the host decides it (ADR-0048
  // decision 8): the focused element for `press`, an element's input type
  // and download attribute for `click`.

  function controlOf(el: Element): RawControl {
    const role = roleOf(el) || '';
    const control: RawControl = { r: role, n: nameOf(el, role).slice(0, MAX_TEXT), d: (el.tagName === 'A' || el.tagName === 'AREA') && el.hasAttribute('download') };
    if (is(el, 'INPUT')) control.t = (el.getAttribute('type') || 'text').toLowerCase();
    return control;
  }

  function focusedControl(): RawControl | InFrame | null {
    const target = deepActive(document);
    if (!target || target === document.body || target === document.documentElement) return null;
    // A cross-origin frame has the focus: the host asks that frame.
    if (target.tagName === 'IFRAME' || target.tagName === 'FRAME') return { in_frame: true };
    return controlOf(target);
  }

  // ---- the entry point -----------------------------------------------------

  async function run(op: string, args: unknown): Promise<Answer> {
    try {
      // An element id is this document's: one taken before a navigation
      // must not name an element of the next document.
      const argDoc = field(args, 'doc');
      if (argDoc !== undefined && argDoc !== doc) throw failure('stale_ref', 'the page changed since the snapshot: snapshot again');
      switch (op) {
        case 'state': return { ok: { doc, url: location.href, title: document.title, ready: document.readyState } };
        case 'snapshot': return { ok: snapshot(field(args, 'max_raw')) };
        case 'text': return { ok: pageText(field(args, 'max_chars')) };
        case 'has_text': return { ok: hasText(String(field(args, 'text'))) };
        case 'focused': return { ok: document.hasFocus() && document.activeElement !== null && document.activeElement !== document.body };
        case 'focused_control': return { ok: focusedControl() };
        case 'inspect': return { ok: controlOf(byId(field(args, 'id'))) };
        case 'resources': return { ok: performance.getEntriesByType('resource').length };
        case 'alive': {
          const el = elements.get(field(args, 'id'));
          return { ok: el !== undefined && el.isConnected };
        }
        case 'rect': return { ok: rectOf(byId(field(args, 'id'))) };
        case 'click': return { ok: await click(field(args, 'id'), field(args, 'double') === true) };
        case 'type': return { ok: typeText(field(args, 'id'), String(field(args, 'text')), field(args, 'clear') === true, field(args, 'submit') === true) };
        case 'select': return { ok: selectValues(field(args, 'id'), strings(field(args, 'values'))) };
        case 'scroll_to': return { ok: scrollTo(field(args, 'id')) };
        case 'scroll_page': return { ok: scrollPage(String(field(args, 'direction')), field(args, 'pixels')) };
        case 'press': return { ok: pressKey(String(field(args, 'key')), strings(field(args, 'modifiers'))) };
        default: throw failure('internal', 'unknown driver operation');
      }
    } catch (error: unknown) {
      const code = field(error, 'code');
      return { error: { code: code || 'internal', message: code ? field(error, 'message') : 'the page script failed' } };
    }
  }

  const api: DriverApi = Object.freeze({ run: (op: string, json: string) => run(op, parse(json)).then((result) => stringify(result)) });
  Object.defineProperty(globalThis, KEY, { value: api });

  // Tell the host this frame exists, so it can call into a cross-origin
  // frame. The handler is registered in THIS world only: the page world's
  // `webkit.messageHandlers` has no such name.
  // The person's own input (ADR-0048 decision 4): a click, a key, a scroll
  // in the page while the agent drives pauses it. Only trusted events count
  // (the page's scripts cannot make one, and the driver's own actions are
  // untrusted DOM events); listened for in the capture phase of this world,
  // so the page cannot stop them first. At most one message every 50 ms.
  let lastInput = 0;
  const reportInput = (event: Event): void => {
    if (!event.isTrusted) return;
    const now = Date.now();
    if (now - lastInput < 50) return;
    lastInput = now;
    try {
      window.webkit.messageHandlers.eliteaBrowserInput.postMessage(event.type);
    } catch (_) {
      // A host without the handler (the lazy install of a call).
    }
  };
  for (const type of ['pointerdown', 'keydown', 'wheel', 'touchstart']) {
    window.addEventListener(type, reportInput, { capture: true, passive: true });
  }

  if (window !== window.top) {
    const announce = (): void => {
      try {
        window.webkit.messageHandlers.eliteaBrowserFrame.postMessage(stringify({ token: frameToken, url: location.href }));
      } catch (_) {
        // A host without the handler (the lazy install of a call).
      }
    };
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', announce, { once: true });
    else announce();
  }
})();
