// Lenient JSON extraction from a model reply: a fenced block, or the first
// balanced object that parses, trying each `{` as a start.

// The balanced `{...}` text that starts at `start`, or null.
function balancedAt(s, start) {
  let depth = 0;
  let inStr = false;
  let esc = false;
  for (let i = start; i < s.length; i++) {
    const ch = s[i];
    if (inStr) {
      if (esc) esc = false;
      else if (ch === '\\') esc = true;
      else if (ch === '"') inStr = false;
      continue;
    }
    if (ch === '"') inStr = true;
    else if (ch === '{') depth++;
    else if (ch === '}' && --depth === 0) return s.slice(start, i + 1);
  }
  return null;
}

function tryParse(text) {
  try {
    const v = JSON.parse(text.trim());
    return v && typeof v === 'object' ? v : null;
  } catch {
    return null;
  }
}

export function parseJsonReply(text) {
  if (!text) return null;
  const s = String(text);
  const fence = s.match(/```(?:json)?\s*([\s\S]*?)```/);
  if (fence) {
    const v = tryParse(fence[1]);
    if (v) return v;
  }
  for (let start = s.indexOf('{'); start >= 0; start = s.indexOf('{', start + 1)) {
    const candidate = balancedAt(s, start);
    if (candidate) {
      const v = tryParse(candidate);
      if (v) return v;
    }
  }
  return tryParse(s);
}
