// Real Go + embedded UI + local upstream. No mocked /api or browser interception.
// CHROME_PATH=/path/to/chrome pnpm test:integration
import assert from 'node:assert/strict';
import { execFile, spawn } from 'node:child_process';
import { createServer } from 'node:http';
import { createServer as createNetServer } from 'node:net';
import { mkdir, mkdtemp, readFile, rm, stat, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import { launchBrowser, stopProcess, waitFor } from './cdp.mjs';

const exec = promisify(execFile);
const web = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const root = path.dirname(web);
const tempRoot = '/tmp/opencode';
assert.ok((await stat(tempRoot)).isDirectory(), 'The approved temporary parent must exist.');
assert.ok(process.env.CHROME_PATH, 'Set CHROME_PATH to an existing Chromium executable.');
const temporary = await mkdtemp(path.join(tempRoot, 'agent-relay-integration-'));
const results = path.join(web, 'test-results', 'integration');
await mkdir(results, { recursive: true });
const adminToken = 'integration-admin-token';
const upstreamToken = 'Bearer integration-model-token';
const configFile = path.join(temporary, 'config.json');
const logDirectory = path.join(temporary, 'logs');
const binary = path.join(temporary, 'agent-relay');
const requestBytes = Buffer.from(JSON.stringify({ model: 'gpt-real', messages: [{ role: 'user', content: '真实请求正文 '.repeat(6000) + 'REAL-REQUEST-END' }] }));
const attemptOne = Buffer.from(JSON.stringify({ error: 'rate limit', detail: 'first attempt '.repeat(5500), marker: 'REAL-ATTEMPT-1-END' }));
const successBytes = Buffer.from(JSON.stringify({ content: '真实响应正文 '.repeat(6000), marker: 'REAL-RESPONSE-END' }));
assert.ok([requestBytes, attemptOne, successBytes].every(bytes => bytes.length > 32768));
const calls = [];
const upstreamFailures = [];
const upstream = createServer(async (request, response) => {
  try {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const payload = Buffer.concat(chunks);
    calls.push({ path: request.url, method: request.method, authorization: request.headers.authorization, payload });
    assert.equal(request.method, 'POST');
    assert.equal(request.url, '/v1/chat/completions');
    assert.equal(request.headers.authorization, upstreamToken);
    assert.deepEqual(payload, requestBytes);
    const first = calls.length === 1;
    const bytes = first ? attemptOne : successBytes;
    response.writeHead(first ? 429 : 200, { 'Content-Type': 'application/json; charset=utf-8', 'Content-Length': bytes.length, 'X-Attempt': first ? 'first-real-attempt' : 'second-real-attempt' });
    response.end(bytes);
  } catch (error) { upstreamFailures.push(String(error)); response.writeHead(500); response.end(String(error)); }
});
let go;
let browser;
let serverLog = '';
let goLaunchError;
let healthDiagnostic = '';

async function reservePort() {
  const server = createNetServer();
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(0, '127.0.0.1', resolve); });
  const port = server.address().port;
  await new Promise(resolve => server.close(resolve));
  assert.notEqual(port, 8080);
  assert.notEqual(port, 18851);
  return port;
}

try {
  // Standalone/repeatable: always embed the current frontend rather than stale dist.
  await exec('pnpm', ['build', '--logLevel', 'warn'], { cwd: web, timeout: 120000, maxBuffer: 10 * 1024 * 1024 });
  await exec('go', ['build', '-o', binary, '.'], { cwd: root, timeout: 120000, maxBuffer: 10 * 1024 * 1024 });
  await new Promise((resolve, reject) => { upstream.once('error', reject); upstream.listen(0, '127.0.0.1', resolve); });
  const upstreamPort = upstream.address().port;
  assert.notEqual(upstreamPort, 8080);
  assert.notEqual(upstreamPort, 18851);
  const port = await reservePort();
  const origin = `http://127.0.0.1:${port}`;
  const initialConfig = {
    listen: `127.0.0.1:${port}`, log_limit: 50, max_request_body_bytes: 67108864,
    max_buffer_bytes: 8388608, max_log_body_bytes: 0, body_sniff_timeout_ms: 250, error_body_timeout_ms: 2000,
    upstreams: [], retry: { max_attempts: 3, base_delay_ms: 5, max_delay_ms: 10, jitter: false,
      retry_on_network_error: false, status_codes: [429], body_regexes: ['(?i)rate limit'] },
  };
  await writeFile(configFile, JSON.stringify(initialConfig, null, 2), { mode: 0o600 });
  go = spawn(binary, ['-config', configFile, '-listen', initialConfig.listen, '-log-dir', logDirectory], {
    cwd: temporary, env: { ...process.env, AGENTROUTER_ADMIN_TOKEN: adminToken }, stdio: ['ignore', 'pipe', 'pipe'],
  });
  go.once('error', error => { goLaunchError = error; });
  go.stdout.on('data', data => { serverLog += data; });
  go.stderr.on('data', data => { serverLog += data; });
  await waitFor(async () => {
    if (goLaunchError) throw goLaunchError;
    if (go.exitCode !== null) throw new Error(`Go exited ${go.exitCode}: ${serverLog}`);
    try {
      const health = await fetch(`${origin}/api/health`, { signal: AbortSignal.timeout(1000) });
      healthDiagnostic = `HTTP ${health.status}: ${await health.text()}`;
      return health.ok;
    } catch (error) { healthDiagnostic = `${error} / ${error.cause ?? ''}`; return false; }
  }, 'real Go health', 20000);
  const adminGet = async route => {
    const response = await fetch(`${origin}/api${route}`, { headers: { Authorization: `Bearer ${adminToken}` }, signal: AbortSignal.timeout(10000) });
    assert.equal(response.status, 200, `${route} real API response`);
    return response.json();
  };
  assert.equal((await fetch(`${origin}/api/config`)).status, 401);

  browser = await launchBrowser({ origin, results, profileParent: temporary });
  const { button, nav, field, fieldValue, clickExpression, command, evaluate, waitText, screenshot, downloads } = browser;
  // Page.reload resolves before navigation settles; wait for the old document's
  // marker to disappear so later evaluates never race the new execution context.
  async function reloadDocument() {
    await evaluate('window.__reloadMarker = 1');
    await command('Page.reload');
    await waitFor(async () => {
      try { return await evaluate('typeof window.__reloadMarker === "undefined" && document.readyState === "complete" && !!document.querySelector(".app-shell")'); }
      catch { return false; }
    }, 'fresh document after reload', 15000);
  }
  await waitText('HTTP 401');
  await button('填写令牌'); await field('管理员令牌', adminToken); await button('应用会话令牌'); await button('完成');
  await waitText('配置已同步');
  await nav('重试规则');
  await field('样本响应正文', 'RATE LIMIT from the real integration test');
  await button('运行匹配测试'); await waitText('匹配成功');
  await field('测试表达式', '(?<=x)y');
  await button('运行匹配测试'); await waitText('测试失败 / 表达式无效');
  assert.ok(browser.apiResponses.some(value => value.url.endsWith('/api/regex/test') && value.status === 200));
  await nav('供应商管理'); await button('添加供应商');
  await waitText('这里不生成业务密钥');
  await field('供应商名称', 'real-local-upstream');
  await field('调用标识符', 'real-local');
  await field('上游根地址', `http://127.0.0.1:${upstreamPort}`);
  // No stored API key step: callers authenticate with the provider's own key.
  assert.equal(await evaluate(`!![...document.querySelectorAll('dialog[open] label')].find(e=>e.textContent.includes('API Key'))`), false, 'provider editor must not ask for API keys');
  await button('应用到草稿');
  await waitText('保存后生效');
  assert.equal(await evaluate(`document.querySelectorAll('[aria-label="复制地址"]').length`), 0, 'unsaved provider must not offer copy');
  await button('保存配置'); await waitText('配置已保存');
  let liveConfig = await adminGet('/config');
  assert.equal(liveConfig.upstreams.length, 1);
  assert.equal(liveConfig.upstreams[0].id, 'real-local');
  assert.equal(liveConfig.upstreams[0].enabled, true);
  assert.ok(!liveConfig.upstreams[0].headers || Object.keys(liveConfig.upstreams[0].headers).length === 0);
  // One-click copy: exact prefix and /v1 addresses for the real random-origin gateway.
  await clickExpression(`document.querySelector('[aria-label="复制地址"]')`);
  await waitText('已复制');
  assert.equal(await evaluate('navigator.clipboard.readText()'), `${origin}/real-local`);
  await clickExpression(`document.querySelector('[aria-label="复制 /v1 地址"]')`);
  const v1URL = await evaluate('navigator.clipboard.readText()');
  assert.equal(v1URL, `${origin}/real-local/v1`);
  assert.equal(await evaluate('document.querySelectorAll("dialog[open]").length'), 0, 'copy must not open the editor');
  await screenshot('real-providers');
  await screenshot('real-providers-mobile', 375, 1400);
  await command('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1080, deviceScaleFactor: 1, mobile: false });

  // Identifier stays read-only and stable across reloads and unrelated edits.
  await reloadDocument(); await waitText('配置已同步'); await nav('供应商管理'); await button('编辑');
  assert.equal(await evaluate(`document.querySelector('dialog[open] .readonly-id')?.textContent`), 'real-local');
  assert.equal(await evaluate(`!![...document.querySelectorAll('dialog[open] input, dialog[open] textarea')].find(e=>e.value==='real-local')`), false, 'persisted identifier is not editable');
  await clickExpression(`document.querySelector('dialog[open] .advanced-settings summary')`);
  await field('路由权重', '125'); await button('应用到草稿'); await button('保存配置'); await waitText('配置已保存');
  liveConfig = await adminGet('/config');
  assert.equal(liveConfig.upstreams[0].id, 'real-local');
  assert.equal(liveConfig.upstreams[0].weight, 125);
  const storedConfig = JSON.parse(await readFile(configFile, 'utf8'));
  assert.equal(storedConfig.upstreams[0].id, 'real-local');
  assert.ok(!storedConfig.upstreams[0].headers?.Authorization, 'no provider key is ever stored through the UI');

  // Only this request reaches the fake upstream. Management/static traffic must not.
  const response = await fetch(`${v1URL}/chat/completions`, {
    method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: upstreamToken },
    body: requestBytes, signal: AbortSignal.timeout(15000),
  });
  assert.equal(response.status, 200);
  assert.deepEqual(Buffer.from(await response.arrayBuffer()), successBytes);
  assert.equal(calls.length, 2, '429 then 200 must issue exactly two upstream calls');
  assert.deepEqual(upstreamFailures, []);
  let entry;
  await waitFor(async () => { entry = (await adminGet('/logs?limit=50')).logs?.[0]; return !!entry; }, 'persisted real request log');
  assert.equal(entry.attempt_count, 2);
  assert.equal(entry.upstream_id, 'real-local');
  assert.equal(entry.attempts[0].status, 429); assert.equal(entry.attempts[1].status, 200);
  assert.equal(entry.attempts[0].url, `http://127.0.0.1:${upstreamPort}/v1/chat/completions`);
  assert.equal(entry.request_body_complete, true); assert.equal(entry.response_body_complete, true);
  assert.equal(entry.request_body_truncated, true); assert.equal(entry.response_body_truncated, true);
  assert.equal(entry.request_body_captured_bytes, requestBytes.length);
  assert.equal(entry.response_body_captured_bytes, successBytes.length);
  assert.ok(!entry.request_body.includes('REAL-REQUEST-END'));
  assert.ok(!entry.response_body.includes('REAL-RESPONSE-END'));
  const realStats = await adminGet('/stats');
  assert.equal(realStats.total_requests, 1); assert.equal(realStats.retried_requests, 1); assert.equal(realStats.retry_success, 1);
  await nav('运行总览');
  // Cover the 30s API timeout plus another 5s poll, not just one scheduling window.
  await waitFor(() => evaluate(`document.querySelector('.stat-value').textContent==='1'`), 'real overview counter', 60000);
  await screenshot('real-overview');
  await nav('请求日志'); await button('刷新'); await waitText('real-local-upstream');
  await clickExpression(`document.querySelector('.table-link')`);
  await waitText('请求时间'); await waitText('预览已省略，完整正文可读取');
  await waitText('X-Attempt: first-real-attempt'); await waitText('X-Attempt: second-real-attempt');
  assert.ok(!browser.resourceURLs.some(url => url.includes('/body/')), 'no raw body request before user action');

  async function readAndDownload(part, expected, marker) {
    const container = `document.querySelector('[data-body-part="${part}"]')`;
    await clickExpression(`[...${container}.querySelectorAll('button')].find(e=>e.textContent.includes('读取完整正文'))`);
    await waitText(marker);
    assert.equal(await evaluate(`${container}.querySelector('pre').textContent`), expected.toString());
    await clickExpression(`[...${container}.querySelectorAll('button')].find(e=>e.textContent.includes('下载原始文件'))`);
    const filename = path.join(downloads, `request-${entry.id}-${part}.bin`);
    await waitFor(async () => { try { return (await readFile(filename)).equals(expected); } catch { return false; } }, `real ${part} byte-exact download`);
  }
  await readAndDownload('attempt-1', attemptOne, 'REAL-ATTEMPT-1-END');
  await readAndDownload('attempt-2', successBytes, 'REAL-RESPONSE-END');
  await button('请求内容'); await readAndDownload('request', requestBytes, 'REAL-REQUEST-END');
  await clickExpression(`document.querySelector('[aria-label="复制请求正文（已读取）"]')`);
  assert.equal(await evaluate('navigator.clipboard.readText()'), requestBytes.toString());
  await button('响应内容'); await readAndDownload('response', successBytes, 'REAL-RESPONSE-END');
  await evaluate(`(() => { const pre=document.querySelector('[data-body-part="response"] pre'); pre.scrollTop=pre.scrollHeight; })()`);
  await screenshot('real-full-body'); await screenshot('real-full-body-mobile', 375, 1000);
  await button('导出元数据');
  const metadataFile = path.join(downloads, `request-${entry.id}-metadata.json`);
  await waitFor(async () => { try { return JSON.parse(await readFile(metadataFile, 'utf8')).id === entry.id; } catch { return false; } }, 'real metadata download');
  assert.equal(await evaluate('localStorage.length'), 0);
  assert.equal(await evaluate('sessionStorage.getItem("agent-relay.admin-token")'), adminToken);
  assert.equal(await evaluate('location.pathname'), '/');
  assert.equal(calls.length, 2, 'body and metadata endpoints must stay in admin routing');
  assert.ok(browser.resourceURLs.every(url => !url.includes(adminToken) && !url.includes(':8080')));
  assert.deepEqual(browser.pageErrors, []);
  await writeFile(path.join(results, 'api-observations.json'), JSON.stringify({
    origin, requests: browser.resourceURLs.filter(url => url.includes('/api/')), responses: browser.apiResponses,
    timings: await evaluate(`performance.getEntriesByType('resource').filter(entry=>entry.name.includes('/api/')).map(entry=>({url:entry.name,duration:entry.duration,transferSize:entry.transferSize}))`),
  }, null, 2));
  console.log(`PASS real-Go integration: ${origin}; actual embedded UI, Bearer login/401, Go RE2 match/error UI, provider created/enabled/saved via UI without any stored key, exact one-click copy of ${origin}/{id} and /v1, copied /v1 URL POST → 429 then 200 exactly 2 calls with stripped path and the caller's own unchanged vendor key, real stats/log metadata, request + response + both attempt bodies >32KiB/end markers, byte-exact downloads, responsive screenshots.`);
  console.log(`Artifacts: ${results}`);
} catch (error) {
  console.error('Go output:', serverLog);
  console.error('Last health result:', healthDiagnostic);
  if (browser) { try { console.error('API requests:', browser.resourceURLs.filter(url => url.includes('/api/'))); console.error('API responses:', browser.apiResponses); console.error('Visibility:', await browser.evaluate('document.visibilityState')); console.error('Page:', await browser.evaluate('document.body.innerText.slice(0,16000)')); await browser.screenshot('real-failure'); } catch {} }
  throw error;
} finally {
  // Close consumers first, then the Go listener and fake upstream, then private files.
  try { await browser?.close(); }
  finally {
    try { await stopProcess(go); }
    finally {
      upstream.closeAllConnections();
      if (upstream.listening) await new Promise(resolve => upstream.close(resolve));
      await writeFile(path.join(results, 'go-server.log'), serverLog);
      await rm(temporary, { recursive: true, force: true });
    }
  }
}
