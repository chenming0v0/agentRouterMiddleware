import test from 'node:test';
import assert from 'node:assert/strict';
import { asNumber, configShape, newProviderID, parseHeaders, providerBaseURL, splitList, suggestID, upstreamIDError, validUpstreamID, validateConfig, validateUpstream, validURL } from '../src/lib.ts';
import { api, apiBlob, ApiError, getToken, storeToken } from '../src/api.ts';
import { captureInfo, captureMessage, captureState, decodeBody } from '../src/body.ts';

const config = {
  listen: ':18851', log_limit: 50, max_request_body_bytes: 67108864,
  max_buffer_bytes: 8388608, max_log_body_bytes: 0, upstreams: null,
  retry: { max_attempts: 3, base_delay_ms: 500, max_delay_ms: 8000, jitter: true,
    retry_on_network_error: true, status_codes: [400, 404, 429, 520], body_regexes: ['(?i)rate limit'] },
};

test('provider identifiers are URL-safe, non-reserved, and drive stable copyable URLs', () => {
  for (const id of ['openai', '9live', 'A-Z_0-9', 'a'.repeat(64)]) {
      assert.equal(validUpstreamID(id), true, id);
      const u = { id, name: 'p', base_url: 'https://gateway.example.com', enabled: true, weight: 100, timeout_ms: 300000, path_globs: [], model_globs: [], headers: {} };
      assert.deepEqual(validateUpstream(u), []);
    }
    // No location under the Node test runner: the URL is root-relative, with no query or secret.
    assert.equal(providerBaseURL('openai'), '/openai');
    assert.equal(providerBaseURL('openai') + '/v1', '/openai/v1');
    for (const bad of ['', '-lead', '_lead', 'bad/id', 'bad id', 'bad.id', 'a'.repeat(65), 'api', 'API', 'Api', 'assets', 'v1', 'V1', 'v1beta', 'V1BETA', 'v2']) {
      assert.equal(validUpstreamID(bad), false, bad);
      assert.ok(upstreamIDError(bad).length > 0);
    }
    assert.match(upstreamIDError('V1'), /保留/);
    const invalid = validateUpstream({ id: 'v1', name: 'p', base_url: 'https://gateway.example.com', enabled: true, weight: 100, timeout_ms: 300000, path_globs: [], model_globs: [], headers: {} });
    assert.match(invalid.join(), /保留的路径前缀/);
    assert.match(validateConfig({ ...config, upstreams: [{ id: 'openai', name: 'a', base_url: 'https://gateway.example.com', enabled: true, weight: 100, timeout_ms: 1, path_globs: [], model_globs: [], headers: {} }, { id: 'openai', name: 'b', base_url: 'https://gateway.example.com', enabled: true, weight: 100, timeout_ms: 1, path_globs: [], model_globs: [], headers: {} }] }).join(), /ID 重复/);
    assert.match(validateConfig({ ...config, upstreams: [{ id: '-bad', name: 'a', base_url: 'https://gateway.example.com', enabled: true, weight: 100, timeout_ms: 1, path_globs: [], model_globs: [], headers: {} }] }).join(), /开头/);
    assert.equal(suggestID('OpenAI 官方服务'), 'openai');
    assert.equal(suggestID('中文服务'), '');
    assert.equal(suggestID('my provider!!'), 'my-provider');
    assert.equal(suggestID('  --9live-- '), '9live');
});

test('provider ID generation survives non-secure contexts and missing Web Crypto', () => {
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, 'crypto');
  try {
    // A public-IP HTTP page is not a secure context: getRandomValues stays
    // available while randomUUID is undefined.
    let calls = 0;
    Object.defineProperty(globalThis, 'crypto', { configurable: true, value: {
      getRandomValues: array => { calls++; for (let i = 0; i < array.length; i++) array[i] = (calls * 37 + i * 11) & 0xff; return array; },
    } });
    assert.equal(typeof globalThis.crypto.randomUUID, 'undefined');
    const first = newProviderID();
    const second = newProviderID();
    assert.match(first, /^p_[0-9a-f]{24}$/);
    assert.ok(first.length <= 64);
    assert.equal(validUpstreamID(first), true);
    assert.equal(validUpstreamID(second), true);
    assert.notEqual(first, second);
    assert.equal(calls, 2);
    // No Web Crypto at all: still a valid, distinct, non-secret route ID.
    Object.defineProperty(globalThis, 'crypto', { configurable: true, value: undefined });
    const ids = new Set([newProviderID(), newProviderID(), newProviderID()]);
    assert.equal(ids.size, 3);
    for (const id of ids) assert.equal(validUpstreamID(id), true, id);
  } finally {
    if (descriptor) Object.defineProperty(globalThis, 'crypto', descriptor);
    else delete globalThis.crypto;
  }
});

test('0 log body cap is valid, positive byte limits and attempts are required', () => {
  assert.deepEqual(validateConfig(config), []);
  assert.match(validateConfig({ ...config, log_limit: 0 }).join(), /保留条数/);
  assert.match(validateConfig({ ...config, max_request_body_bytes: 0 }).join(), /请求体上限/);
  assert.match(validateConfig({ ...config, max_log_body_bytes: -1 }).join(), /日志正文上限/);
  assert.match(validateConfig({ ...config, retry: { ...config.retry, max_attempts: 0 } }).join(), /总尝试次数/);
  assert.match(validateConfig({ ...config, retry: { ...config.retry, max_delay_ms: 100 } }).join(), /不能小于/);
});

test('arbitrary valid HTTP codes are allowed, invalid codes are rejected', () => {
  assert.deepEqual(validateConfig({ ...config, retry: { ...config.retry, status_codes: [100, 201, 418, 520, 599] } }), []);
  for (const code of [0, 99, 600, 429.5, NaN]) {
    assert.match(validateConfig({ ...config, retry: { ...config.retry, status_codes: [code] } }).join(), /HTTP 状态码/);
  }
});

test('Go RE2 expressions are not rejected by JavaScript regex validation', () => {
  assert.deepEqual(validateConfig({ ...config, retry: { ...config.retry, body_regexes: ['(?i)rate limit', '(?P<name>channel)'] } }), []);
  assert.match(validateConfig({ ...config, retry: { ...config.retry, body_regexes: [''] } }).join(), /不能为空/);
});

test('list editing accepts commas, Chinese commas and newlines, without empty entries', () => {
  assert.deepEqual(splitList(' /v1/*, /v2/*\n/v1/*， /v3/*\n'), ['/v1/*', '/v2/*', '/v3/*']);
  assert.deepEqual(splitList(''), []);
  assert.ok(Number.isNaN(asNumber('')));
});

test('headers editor validates JSON object, string values and header injection', () => {
  assert.deepEqual(parseHeaders('{"Authorization":"Bearer example","X-Tenant":"one"}'), { Authorization: 'Bearer example', 'X-Tenant': 'one' });
  for (const value of ['[]', 'null', '{"X":10}', '{"bad name":"x"}', '{"X":"x\\r\\nInjected: yes"}', '{']) assert.throws(() => parseHeaders(value));
});

test('upstream validation keeps zero weights and rejects missing name/invalid URLs', () => {
  const u = { id: 'u_test', name: 'main', base_url: 'https://gateway.example.com', enabled: true, weight: 0, timeout_ms: 300000, path_globs: [], model_globs: [], headers: {} };
  assert.deepEqual(validateUpstream(u), []);
  assert.match(validateUpstream({ ...u, name: '' }).join(), /名称/);
  assert.match(validateUpstream({ ...u, timeout_ms: 0 }).join(), /超时/);
  assert.equal(validURL('javascript:alert(1)'), false);
  assert.equal(validURL('https://gateway.example.com'), true);
});

test('config shape fails closed, without creating default data', () => {
  assert.equal(configShape(config), true);
  for (const value of [null, {}, { ok: true }, '<html>']) assert.equal(configShape(value), false);
});

test('API token is session-only, sent in Authorization, and 401/403 explain access policy', async () => {
  const originalFetch = globalThis.fetch;
  const originalWindow = globalThis.window;
  const originalStorage = globalThis.sessionStorage;
  const values = new Map();
  globalThis.sessionStorage = { getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: key => values.delete(key) };
  globalThis.window = new EventTarget();
  let eventStatus;
  window.addEventListener('relay:auth-error', event => { eventStatus = event.detail.status; });
  try {
    storeToken('  session-secret  ');
    assert.equal(getToken(), 'session-secret');
    globalThis.fetch = async (url, options) => {
      assert.equal(url, '/api/config');
      assert.equal(options.headers.get('Authorization'), 'Bearer session-secret');
      assert.ok(!url.includes('secret'));
      return new Response(JSON.stringify(config), { status: 200 });
    };
    assert.deepEqual(await api('/config'), config);
    for (const status of [401, 403]) {
      globalThis.fetch = async () => new Response(JSON.stringify({ error: 'denied' }), { status });
      await assert.rejects(api('/config'), error => error instanceof ApiError && error.status === status && error.message.includes('AGENTROUTER_ADMIN_TOKEN'));
      assert.equal(eventStatus, status);
    }
    storeToken('');
    assert.equal(getToken(), '');
  } finally {
    globalThis.fetch = originalFetch;
    globalThis.window = originalWindow;
    globalThis.sessionStorage = originalStorage;
  }
});

test('successful non-JSON API response is rejected rather than accepted as config', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () => new Response('<html>wrong upstream</html>', { status: 200 });
  try { await assert.rejects(api('/config'), /没有返回 JSON/); }
  finally { globalThis.fetch = originalFetch; }
});

test('strict backend bounds reject values instead of silently clamping them', () => {
  for (const [key, invalid] of Object.entries({ log_limit: [0, 1001], max_request_body_bytes: [0, 1073741825], max_buffer_bytes: [0, 1073741825], max_log_body_bytes: [-1, 1073741825], body_sniff_timeout_ms: [0, 60001], error_body_timeout_ms: [0, 60001] })) {
    for (const value of invalid) assert.ok(validateConfig({ ...config, [key]: value }).length, `${key}=${value}`);
  }
  const bounded = { ...config, log_limit: 1000, max_request_body_bytes: 1073741824, max_buffer_bytes: 1073741824, max_log_body_bytes: 1073741824, body_sniff_timeout_ms: 60000, error_body_timeout_ms: 1 };
  assert.deepEqual(validateConfig(bounded), []);
  assert.deepEqual(validateConfig({ ...config, retry: { ...config.retry, max_attempts: 20 } }), []);
  for (const retry of [{ max_attempts: 21 }, { base_delay_ms: 3600001 }, { max_delay_ms: 3600001 }]) assert.ok(validateConfig({ ...config, retry: { ...config.retry, ...retry } }).length);
  const upstream = { id: 'u_limits', name: 'limits', base_url: 'https://gateway.example.com', weight: 1000000, timeout_ms: 86400000 };
  assert.deepEqual(validateUpstream(upstream), []);
  assert.ok(validateUpstream({ ...upstream, weight: 1000001 }).length);
  assert.ok(validateUpstream({ ...upstream, timeout_ms: 86400001 }).length);
  assert.equal(validURL('https://user:pass@gateway.example.com'), false);
  assert.equal(validURL('https://gateway.example.com/#part'), false);
  assert.deepEqual(parseHeaders('{"Authorization":"***"}'), { Authorization: '***' });
});

test('shortened preview does not imply incomplete disk capture', () => {
  const full = captureInfo({ response_body_bytes: 65536, response_body_captured_bytes: 65536, response_body_complete: true, response_body_available: true }, 'response');
  assert.equal(captureState(full), 'complete');
  assert.match(captureMessage(full, true), /预览已省略，完整正文可读取/);
  const partial = { ...full, complete: false, captured: 32768, error: 'capture limit reached' };
  assert.equal(captureState(partial), 'partial');
  assert.match(captureMessage(partial, true), /实际捕获不完整.*capture limit reached/);
  assert.match(captureMessage({ available: false, error: 'disk I\/O error' }, true), /不可用.*disk I\/O error/);
  assert.equal(captureState({}), 'unknown');
  assert.match(captureMessage({}, true), /未提供完整捕获状态/);
});

test('body viewer preserves all text including end marker and refuses binary/compressed decoding', async () => {
  const text = '正文\n'.repeat(20000) + 'END-OF-COMPLETE-BODY';
  const blob = new Blob([text]);
  assert.equal(await decodeBody(blob, { 'Content-Type': ['application/json; charset=utf-8'] }), text);
  assert.equal(await decodeBody(blob, { 'Content-Encoding': ['gzip'] }), null);
  assert.equal(await decodeBody(new Blob([new Uint8Array([0, 255, 128, 65])]), {}), null);
  assert.equal(await decodeBody(blob, { 'Content-Type': ['application/octet-stream'] }), null);
});

test('raw API keeps bytes, Bearer authorization, abort signal and actionable errors', async () => {
  const original = { fetch: globalThis.fetch, window: globalThis.window, sessionStorage: globalThis.sessionStorage };
  globalThis.sessionStorage = { getItem: () => 'raw-session-token' };
  globalThis.window = new EventTarget();
  const bytes = new Uint8Array([0, 255, 128, 13, 10, 65]);
  const controller = new AbortController();
  try {
    globalThis.fetch = async (url, options) => {
      assert.equal(url, '/api/logs/id/body/attempt-1');
      assert.equal(options.headers.get('Authorization'), 'Bearer raw-session-token');
      assert.equal(options.headers.get('Accept'), 'application/octet-stream');
      assert.ok(options.signal);
      return new Response(bytes, { headers: { 'Content-Type': 'application/octet-stream', 'Content-Length': String(bytes.length) } });
    };
    assert.deepEqual(new Uint8Array(await (await apiBlob('/logs/id/body/attempt-1', controller.signal)).arrayBuffer()), bytes);
    for (const status of [401, 403, 404]) {
      globalThis.fetch = async () => new Response('{"error":"denied"}', { status });
      await assert.rejects(apiBlob('/logs/id/body/request'), e => e instanceof ApiError && e.status === status && e.message.includes(status === 404 ? '淘汰' : 'AGENTROUTER_ADMIN_TOKEN'));
    }
    controller.abort();
    globalThis.fetch = async (_url, options) => { options.signal.throwIfAborted(); };
    await assert.rejects(apiBlob('/logs/id/body/request', controller.signal), e => e.name === 'AbortError');
  } finally { Object.assign(globalThis, original); }
});
