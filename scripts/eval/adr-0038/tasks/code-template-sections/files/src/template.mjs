// Tiny mustache-style template renderer.

/**
 * @param {string} template
 * @param {object} data
 * @returns {string}
 */
export function render(template, data) {
  return template.replace(/\{\{(\w+)\}\}/g, (_, name) => {
    const v = data[name];
    return v === undefined ? '' : String(v);
  });
}
