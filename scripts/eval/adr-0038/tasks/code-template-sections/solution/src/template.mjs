// Tiny mustache-style template renderer.

const TAG_RE = /\{\{\{\s*([^}]*?)\s*\}\}\}|\{\{\s*([#^/!&]?)\s*([\s\S]*?)\s*\}\}/g;

const ESCAPES = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
const escapeHtml = (s) => s.replace(/[&<>"']/g, (c) => ESCAPES[c]);

function parseTemplate(template) {
  const root = { children: [] };
  const stack = [root];
  let last = 0;
  for (const m of template.matchAll(TAG_RE)) {
    const parent = stack[stack.length - 1];
    if (m.index > last) parent.children.push({ type: 'text', value: template.slice(last, m.index) });
    last = m.index + m[0].length;

    if (m[1] !== undefined) {
      parent.children.push({ type: 'var', name: m[1], raw: true });
      continue;
    }
    const sigil = m[2];
    const name = m[3];
    switch (sigil) {
      case '!':
        break;
      case '&':
        parent.children.push({ type: 'var', name, raw: true });
        break;
      case '#':
      case '^': {
        const node = { type: sigil === '#' ? 'section' : 'inverted', name, children: [] };
        parent.children.push(node);
        stack.push(node);
        break;
      }
      case '/': {
        const open = stack.pop();
        if (open === root || open.name !== name) {
          throw new Error(`unexpected closing tag {{/${name}}}`);
        }
        break;
      }
      default:
        parent.children.push({ type: 'var', name, raw: false });
    }
  }
  if (stack.length > 1) throw new Error(`unclosed section {{#${stack[stack.length - 1].name}}}`);
  if (last < template.length) root.children.push({ type: 'text', value: template.slice(last) });
  return root.children;
}

function lookup(stack, name) {
  if (name === '.') return stack[stack.length - 1];
  const [head, ...rest] = name.split('.');
  for (let i = stack.length - 1; i >= 0; i--) {
    const ctx = stack[i];
    if (ctx !== null && typeof ctx === 'object' && Object.prototype.hasOwnProperty.call(ctx, head)) {
      let value = ctx[head];
      for (const part of rest) {
        if (value === null || value === undefined) return undefined;
        value = value[part];
      }
      return value;
    }
  }
  return undefined;
}

const isEmpty = (v) => !v || (Array.isArray(v) && v.length === 0);

function renderNodes(nodes, stack) {
  let out = '';
  for (const node of nodes) {
    if (node.type === 'text') {
      out += node.value;
    } else if (node.type === 'var') {
      const v = lookup(stack, node.name);
      if (v === null || v === undefined) continue;
      const s = String(v);
      out += node.raw ? s : escapeHtml(s);
    } else if (node.type === 'section') {
      const v = lookup(stack, node.name);
      if (isEmpty(v)) continue;
      if (Array.isArray(v)) {
        for (const item of v) out += renderNodes(node.children, [...stack, item]);
      } else {
        out += renderNodes(node.children, [...stack, v]);
      }
    } else if (node.type === 'inverted') {
      if (isEmpty(lookup(stack, node.name))) out += renderNodes(node.children, stack);
    }
  }
  return out;
}

/**
 * @param {string} template
 * @param {object} data
 * @returns {string}
 */
export function render(template, data) {
  return renderNodes(parseTemplate(template), [data]);
}
