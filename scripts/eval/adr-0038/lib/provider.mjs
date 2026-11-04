// OpenAI-compatible chat client with one thinking switch per call.
// A provider is one entry of providers.json. The thinking control kinds
// follow ADR-0038 decision 2: effort, budget, toggle.
import { readFileSync } from 'node:fs';
import http from 'node:http';
import https from 'node:https';

export function loadProviders(path) {
  return JSON.parse(readFileSync(path, 'utf8'));
}

// Body fields that turn thinking on or off for one request.
export function thinkingParams(thinking, on) {
  const t = thinking ?? { kind: 'toggle' };
  switch (t.kind) {
    case 'toggle':
      return { chat_template_kwargs: { enable_thinking: Boolean(on) } };
    case 'effort':
      return { reasoning_effort: on ? (t.on ?? 'medium') : (t.off ?? 'none') };
    case 'budget': {
      const budget = t.budget ?? 4096;
      if (t.style === 'gemini') {
        return { extra_body: { google: { thinking_config: { thinking_budget: on ? budget : 0 } } } };
      }
      // Anthropic: thinking omitted means off.
      return on ? { thinking: { type: 'enabled', budget_tokens: budget } } : {};
    }
    case 'fixed_on':
    case 'fixed_off':
      return {};
    default:
      throw new Error(`unknown thinking kind: ${t.kind}`);
  }
}

export function buildBody(provider, { messages, tools, thinking, maxTokens }) {
  const body = {
    model: provider.model,
    messages,
    ...thinkingParams(provider.thinking, thinking),
  };
  if (provider.temperature !== null && provider.temperature !== undefined) {
    body.temperature = provider.temperature;
  }
  body[provider.max_tokens_field ?? 'max_tokens'] = maxTokens ?? provider.max_tokens ?? 8192;
  if (tools && tools.length) body.tools = tools;
  return body;
}

export function parseUsage(usage) {
  const u = usage ?? {};
  const reasoning =
    u.completion_tokens_details?.reasoning_tokens ?? u.output_tokens_details?.reasoning_tokens ?? 0;
  return {
    prompt: u.prompt_tokens ?? 0,
    completion: u.completion_tokens ?? 0,
    reasoning,
  };
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// node:http instead of fetch: fetch (undici) drops a request whose response
// headers take longer than 300 s, and a long non-streamed generation under
// load does take that long.
function postJson(url, body, key, timeoutMs) {
  const u = new URL(url);
  const lib = u.protocol === 'https:' ? https : http;
  const payload = JSON.stringify(body);
  return new Promise((resolve, reject) => {
    const req = lib.request(u, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        'content-length': Buffer.byteLength(payload),
        ...(key ? { authorization: `Bearer ${key}` } : {}),
      },
    }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => resolve({ status: res.statusCode, text: Buffer.concat(chunks).toString('utf8') }));
      res.on('error', reject);
    });
    req.setTimeout(timeoutMs, () => req.destroy(new Error(`request timed out after ${timeoutMs} ms`)));
    req.on('error', reject);
    req.end(payload);
  });
}

// Returns a chat(messages, opts) function bound to one provider.
// The result: { message, finish, usage: {prompt, completion, reasoning}, ms }.
export function makeClient(provider, { timeoutMs = 1_800_000, retries = 3 } = {}) {
  const key = provider.key_env ? process.env[provider.key_env] : null;
  if (provider.key_env && !key) {
    throw new Error(`provider needs env ${provider.key_env}`);
  }
  const url = provider.base_url.replace(/\/$/, '') + '/chat/completions';
  return async function chat(messages, opts = {}) {
    const body = buildBody(provider, { messages, ...opts });
    let lastErr;
    for (let attempt = 0; attempt <= retries; attempt++) {
      const t0 = Date.now();
      try {
        const res = await postJson(url, body, key, timeoutMs);
        const text = res.text;
        if (res.status < 200 || res.status >= 300) {
          const err = new Error(`HTTP ${res.status}: ${text.slice(0, 500)}`);
          err.status = res.status;
          throw err;
        }
        const data = JSON.parse(text);
        const choice = data.choices?.[0] ?? {};
        return {
          message: choice.message ?? { role: 'assistant', content: '' },
          finish: choice.finish_reason ?? 'stop',
          usage: parseUsage(data.usage),
          ms: Date.now() - t0,
        };
      } catch (err) {
        lastErr = err;
        // A 4xx other than 429 does not get better on retry.
        if (err.status && err.status < 500 && err.status !== 429) throw err;
        await sleep(2000 * (attempt + 1));
      }
    }
    throw lastErr;
  };
}
