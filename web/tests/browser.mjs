// Browser-level UI contract checks, without Playwright or a running Go backend.
// Fixtures exist ONLY in this test server; the shipped app always calls /api.
// Run: CHROME_PATH=/path/to/chrome pnpm test:browser (Node 22+ with WebSocket).
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { gzipSync } from 'node:zlib';
import { launchBrowser, sleep, waitFor } from './cdp.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const results = path.join(root, 'test-results');

let config = {
  listen: ':18851', log_limit: 50, max_request_body_bytes: 67108864,
  max_buffer_bytes: 8388608, max_log_body_bytes: 0, future_security: { preserve: true },
  body_sniff_timeout_ms: 250, error_body_timeout_ms: 2000,
  upstreams: [{ id: 'openai-main', name: 'test-upstream', base_url: 'https://gateway.example.com',
    enabled: false, weight: 100, timeout_ms: 300000, path_globs: ['/v1/*'], model_globs: [],
    headers: { Authorization: '***' }, future_upstream_option: { preserve: true } }],
  retry: { max_attempts: 3, base_delay_ms: 500, max_delay_ms: 8000, jitter: true,
    retry_on_network_error: false, status_codes: [400, 404, 429, 500, 502, 503, 504],
    body_regexes: ['(?i)rate limit'], future_retry_option: { preserve: true } },
};
const stats = { total_requests: 23, retried_requests: 7, retry_success: 5, failed_requests: 2, uptime_seconds: 7265 };
const fullBodies = {
  request: Buffer.from(JSON.stringify({ messages: [{ role: 'user', content: 'request payload '.repeat(5000) + 'REQUEST-END-MARKER' }] })),
  response: Buffer.from('data: {"content":"原始正文"}\n'.repeat(2500) + 'RESPONSE-END-MARKER'),
  'attempt-1': Buffer.from('rate limit '.repeat(6000) + 'ATTEMPT-1-END-MARKER'),
  'attempt-2': Buffer.from('response '.repeat(8000) + 'ATTEMPT-2-END-MARKER'),
};
const captureFields = (prefix, body, complete = true, observed = body.length) => ({
  [`${prefix}_body_bytes`]: observed, [`${prefix}_body_captured_bytes`]: body.length,
  [`${prefix}_body_complete`]: complete, [`${prefix}_body_available`]: true,
});
const log = {
  id: 'browser-contract-log', time: '2026-09-12T10:00:00.123456789Z', method: 'POST',
  path: '/v1/chat/completions', query: 'trace=true', model: 'gpt-test', client_ip: '127.0.0.1',
  status: 200, duration_ms: 1352, attempt_count: 2, retried: true, upstream_id: 'openai-main',
  upstream_name: 'test-upstream', request_headers: { Authorization: ['[REDACTED]'], 'X-Test': ['first', 'second'] },
  request_body: fullBodies.request.subarray(0, 32768).toString(), request_body_truncated: true,
  ...captureFields('request', fullBodies.request),
  response_headers: { 'Content-Type': ['text/event-stream'] },
  response_body: fullBodies.response.subarray(0, 32768).toString(), response_body_truncated: true, streaming: true,
  ...captureFields('response', fullBodies.response), response_body_part: 'attempt-2', regex_skipped: true,
  attempts: [{ upstream_id: 'openai-main', upstream_name: 'test-upstream', url: 'https://gateway.example.com/v1/chat/completions',
    status: 429, duration_ms: 100, error: '', retry_reason: 'status code 429', response_body: 'rate limit', response_body_truncated: true,
    response_headers: { 'X-Attempt': ['one'], 'Content-Type': ['text/plain'] }, ...captureFields('response', fullBodies['attempt-1']) },
  { upstream_id: 'openai-main', upstream_name: 'test-upstream', url: 'https://gateway.example.com/v1/chat/completions',
    status: 200, duration_ms: 750, error: '', retry_reason: '', response_body: 'response preview', response_body_truncated: true,
    response_headers: { 'X-Attempt': ['two'] }, ...captureFields('response', fullBodies['attempt-2']) }],
};
const partialBytes = Buffer.from('partial capture bytes');
const compressedBytes = gzipSync(fullBodies.response);
const partialLog = { ...log, id: 'partial-capture', streaming: false, incomplete: true, error: 'client canceled', attempts: [],
  response_body: 'partial preview', ...captureFields('response', partialBytes, false, 70000), response_body_error: 'capture limit reached' };
const binaryLog = { ...log, id: 'binary-capture', attempts: [], streaming: false, response_body: 'compressed preview',
  response_headers: { 'Content-Encoding': ['gzip'], 'Content-Type': ['application/json'] }, ...captureFields('response', compressedBytes) };
const bodyFiles = { [log.id]: fullBodies, [partialLog.id]: { response: partialBytes }, [binaryLog.id]: { response: compressedBytes } };
let bodyMode = '';
let abortedBodies = 0;
let entries = null;
let auth = 0;
let configError = false;
let rejectSave = false;
const requests = [];
const puts = [];
let cleared = false;
async function body(request) { let text = ''; for await (const chunk of request) text += chunk; return text ? JSON.parse(text) : {}; }
const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://localhost');
  const json = (data, status = 200) => { response.writeHead(status, { 'Content-Type': 'application/json' }); response.end(JSON.stringify(data)); };
  try {
    if (url.pathname.startsWith('/api/')) {
      requests.push({ method: request.method, url: url.pathname + url.search, authorization: request.headers.authorization });
      if (url.pathname === '/api/health') return json({ ok: true });
      if (auth && request.headers.authorization !== 'Bearer browser-session-token') return json({ error: 'access denied' }, auth);
      if (url.pathname === '/api/config' && request.method === 'GET') {
        const masked = structuredClone(config);
        for (const upstream of masked.upstreams) for (const key of Object.keys(upstream.headers)) upstream.headers[key] = '***';
        return configError ? json({ error: 'configuration unavailable' }, 503) : json(masked);
      }
      if (url.pathname === '/api/config' && request.method === 'PUT') {
        const next = await body(request);
        if (rejectSave) return json({ error: 'server rejected configuration' }, 400);
        puts.push(next); config = next;
        await new Promise(resolve => setTimeout(resolve, 200));
        return json({ ok: true });
      }
      if (url.pathname === '/api/stats') return json(stats);
      if (url.pathname === '/api/logs' && request.method === 'GET') return json({ logs: entries, total: entries?.length ?? 0 });
      if (url.pathname === '/api/logs' && request.method === 'DELETE') { entries = []; cleared = true; return json({ ok: true }); }
      const detail = url.pathname.match(/^\/api\/logs\/([^/]+)(?:\/body\/(request|response|attempt-\d+))?$/);
      if (detail) {
        const [, id, part] = detail;
        if (!part) return json([log, partialLog, binaryLog].find(entry => entry.id === id) ?? {}, id === 'network-failure' ? 404 : 200);
        if (bodyMode === 'auth' && request.headers.authorization !== 'Bearer browser-session-token') return json({ error: 'token required' }, 401);
        const bytes = bodyFiles[id]?.[part];
        if (bodyMode === 'missing' || !bytes) return json({ error: 'body not available' }, 404);
        response.writeHead(200, { 'Content-Type': 'application/octet-stream', 'Content-Length': String(bytes.length), 'Cache-Control': 'no-store' });
        if (bodyMode === 'slow') {
          response.write(bytes.subarray(0, 8));
          const timer = setTimeout(() => response.end(bytes.subarray(8)), 5000);
          response.on('close', () => { clearTimeout(timer); if (!response.writableEnded) abortedBodies++; });
          return;
        }
        response.end(bytes); return;
      }
      if (url.pathname === '/api/upstreams/test') { await body(request); return json({ ok: true, status: 401, latency_ms: 22 }); }
      if (url.pathname === '/api/regex/test') {
        const { pattern, text } = await body(request);
        // Verify frontend handling of server results, not the RE2 engine itself.
        if (pattern === '(?<=x)y') return json({ error: 'invalid or unsupported Perl syntax: (?<' }, 400);
        return json({ matched: text.toLowerCase().includes('rate limit') });
      }
      return json({ error: 'unexpected test endpoint' }, 404);
    }
    const target = path.resolve(root, 'dist', url.pathname === '/' ? 'index.html' : `.${url.pathname}`);
    assert.ok(target.startsWith(path.join(root, 'dist') + path.sep));
    const data = await readFile(target);
    response.writeHead(200, { 'Content-Type': ({ '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' })[path.extname(target)] ?? 'application/octet-stream' });
    response.end(data);
  } catch (error) { response.writeHead(500); response.end(String(error)); }
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
let browser;
try {
  browser = await launchBrowser({ origin, results });
  const { command, evaluate, waitText, clickExpression, button, nav, field, screenshot, downloads, pageErrors, resourceURLs } = browser;
  await waitText('配置已同步');
  await waitText('接入第一个供应商');
  await waitText('等待第一条请求');
  assert.equal(await evaluate(`document.querySelector('.stat-value').textContent`), '23');
  await screenshot('overview-desktop');
  await screenshot('overview-mobile', 375, 950);
  await screenshot('overview-tablet', 768, 1050);
  await screenshot('overview-laptop', 1024, 1000);
  await command('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1080, deviceScaleFactor: 1, mobile: false });

  await nav('供应商管理');
  // Saved providers expose the dedicated address with both copy buttons.
  await waitText('/openai-main');
  const clipboardText = () => evaluate('navigator.clipboard.readText()');
  await clickExpression(`document.querySelector('[aria-label="复制地址"]')`);
  assert.equal(await clipboardText(), `${origin}/openai-main`);
  await clickExpression(`document.querySelector('[aria-label="复制 /v1 地址"]')`);
  assert.equal(await clipboardText(), `${origin}/openai-main/v1`);
  assert.ok(await evaluate(`document.querySelectorAll('dialog[open]').length`) === 0, 'copy must not open the editor');
  // Copy failure offers a visible manual fallback instead of fake success.
  await evaluate(`Object.defineProperty(navigator.clipboard, 'writeText', { value: () => Promise.reject(new Error('denied')), configurable: true })`);
  await clickExpression(`document.querySelector('[aria-label="复制地址"]')`);
  await waitText('复制失败，请手动选择内容');
  assert.equal(await evaluate('document.querySelectorAll("dialog[open]").length'), 0, 'failed copy must not open the editor');
  await command('Page.reload'); await waitText('配置已同步'); await nav('供应商管理');
  // Non-secure origin contract: randomUUID may be absent while getRandomValues remains.
  await evaluate(`delete Crypto.prototype.randomUUID`);
  assert.equal(await evaluate('typeof crypto.randomUUID'), 'undefined');
  assert.equal(await evaluate('typeof crypto.getRandomValues'), 'function');

  await button('编辑');
  await field('供应商名称', 'preserved-upstream');
  await button('应用到草稿');
  await waitText('有未保存的配置修改');
  await button('添加供应商');
  assert.match(await evaluate(`document.getElementById([...document.querySelectorAll('label')].find(e=>e.textContent==='调用标识符').htmlFor).value`), /^p_[0-9a-f]{24}$/, 'generated ID must not need randomUUID');
  // Reserved ID is refused in the editor before the draft is touched.
  await field('调用标识符', 'v1');
  await field('供应商名称', 'bad-id-provider');
  await button('应用到草稿');
  await waitText('保留的路径前缀');
  assert.equal(await evaluate('document.querySelectorAll(".upstream-card").length'), 1);
  // Duplicate ID (case-insensitive) is refused before entering the draft.
  await field('调用标识符', 'OPENAI-MAIN');
  await button('应用到草稿');
  await waitText('已被其他供应商占用');
  await field('供应商名称', 'new-upstream');
  await field('调用标识符', 'new-upstream');
  await field('上游根地址', 'https://gateway.example.com');
  await screenshot('upstream-editor');
  await button('应用到草稿');
  // Unsaved providers carry a clear pending state and no fake copy buttons.
  await waitText('保存后生效');
  assert.equal(await evaluate(`document.querySelectorAll('[aria-label="复制 /v1 地址"]').length`), 1, 'only the saved provider offers copy');
  assert.equal(await evaluate('document.querySelectorAll(".upstream-card").length'), 2);
  // Legacy fields stay editable in the collapsed compatibility section.
  await clickExpression(`( () => { const cards=[...document.querySelectorAll('.upstream-card')]; return [...cards.at(-1).querySelectorAll('button')].find(e=>e.textContent.trim()==='编辑'); })()`);
  await clickExpression(`document.querySelector('dialog[open] .advanced-settings summary')`);
  await field('路由权重', '75');
  await field('路径通配规则', '/v1/*, /v2/*');
  await field('模型通配规则', 'gpt-*\nclaude-*');
  await field('附加请求头（JSON）', '{"Authorization":42}');
  await button('应用到草稿');
  await waitText('值必须是字符串');
  await field('附加请求头（JSON）', '{"Authorization":"Bearer upstream-only"}');
  await button('应用到草稿');
  await button('测试连通性');
  await waitText('可达 · HTTP 401');
  assert.equal(await evaluate('document.querySelectorAll("dialog[open]").length'), 0, 'connectivity test must not reopen the editor');
  await screenshot('upstreams-desktop');
  await screenshot('upstreams-mobile', 375, 1500);
  await command('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1080, deviceScaleFactor: 1, mobile: false });

  await nav('重试规则');
  await waitText('3 次总尝试 = 最多 2 次重试');
  await clickExpression(`[...document.querySelectorAll('.code-checkbox')].find(e=>e.textContent.includes('400')).querySelector('.checkbox__content')`);
  await field('其他 HTTP 状态码', '600');
  await button('添加'); await waitText('请输入 100–599');
  await field('其他 HTTP 状态码', '520'); await button('添加');
  await field('样本响应正文', 'RATE LIMIT: this is a browser contract test');
  await button('运行匹配测试'); await waitText('匹配成功');
  await field('样本响应正文', 'healthy response');
  await button('运行匹配测试'); await waitText('未匹配');
  await field('测试表达式', '(?<=x)y');
  await button('运行匹配测试'); await waitText('invalid or unsupported Perl syntax');
  await screenshot('retry-desktop');
  await screenshot('retry-mobile', 375, 1000);
  await command('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1080, deviceScaleFactor: 1, mobile: false });
  // Let both polling intervals run while the config is dirty.
  await sleep(5300);
  await waitText('有未保存的配置修改');
  await button('保存配置');
  assert.ok(await evaluate(`[...document.querySelectorAll('button')].find(e=>e.textContent.includes('保存配置')).disabled`));
  await waitText('配置已保存');
  assert.equal(puts.length, 1);
  const saved = puts[0];
  assert.deepEqual(saved.future_security, { preserve: true });
  assert.deepEqual(saved.retry.future_retry_option, { preserve: true });
  assert.deepEqual(saved.upstreams[0].future_upstream_option, { preserve: true });
  assert.equal(saved.upstreams[0].headers.Authorization, '***');
  assert.equal(saved.upstreams[0].name, 'preserved-upstream');
  assert.ok(saved.retry.status_codes.includes(520));
  assert.ok(!saved.retry.status_codes.includes(400));
  assert.equal(saved.upstreams.length, 2);
  assert.equal(saved.upstreams[1].id, 'new-upstream');
  assert.deepEqual(saved.upstreams[1].path_globs, ['/v1/*', '/v2/*']);
  assert.deepEqual(saved.upstreams[1].model_globs, ['gpt-*', 'claude-*']);
  assert.equal(saved.upstreams[1].headers.Authorization, 'Bearer upstream-only');

  entries = [log, { ...log, id: 'network-failure', status: 0, attempt_count: 1, retried: false, attempts: [], response_body: '', response_body_truncated: false }];
  await nav('请求日志'); await button('刷新'); await waitText('网络错误');
  await screenshot('logs-desktop');
  await field('搜索日志', 'browser-contract-log');
  await waitText('1 条匹配');
  await clickExpression(`document.querySelector('.table-link')`);
  await waitText('预览已省略，完整正文可读取');
  assert.equal(requests.filter(r => r.url.includes('/body/')).length, 0, 'opening a detail must not fetch any raw body');
  await waitText('status code 429');
  await waitText('X-Attempt: one'); await waitText('X-Attempt: two');
  await screenshot('log-detail-desktop');
  const bodyAction = (part, action) => clickExpression(`[...document.querySelector('[data-body-part="${part}"]').querySelectorAll('button')].find(e=>e.textContent.includes(${JSON.stringify(action)}))`);
  const bodyText = part => evaluate(`document.querySelector('[data-body-part="${part}"] pre').textContent`);
  async function checkDownload(id, part, expected) {
    const file = path.join(downloads, `request-${id}-${part}.bin`);
    await waitFor(async () => { try { return (await readFile(file)).equals(expected); } catch { return false; } }, `${part} raw bytes download`);
  }
  await bodyAction('attempt-1', '读取完整正文'); await waitText('ATTEMPT-1-END-MARKER');
  assert.equal(await bodyText('attempt-1'), fullBodies['attempt-1'].toString());
  await bodyAction('attempt-1', '下载原始文件'); await checkDownload(log.id, 'attempt-1', fullBodies['attempt-1']);
  await button('请求内容');
  await waitText('X-Test: first'); await waitText('X-Test: second');
  bodyMode = 'auth';
  await bodyAction('request', '读取完整正文'); await waitText('管理员令牌缺失或无效');
  await button('访问设置'); await field('管理员令牌', 'browser-session-token'); await button('应用会话令牌'); await button('完成');
  await bodyAction('request', '读取完整正文'); await waitText('REQUEST-END-MARKER');
  assert.equal(await bodyText('request'), fullBodies.request.toString());
  assert.ok(requests.some(r => r.url.includes('/body/request') && r.authorization === 'Bearer browser-session-token'));
  bodyMode = '';
  await clickExpression(`document.querySelector('[aria-label="复制请求正文（已读取）"]')`);
  assert.equal(await evaluate('navigator.clipboard.readText()'), fullBodies.request.toString());
  await bodyAction('request', '下载原始文件'); await checkDownload(log.id, 'request', fullBodies.request);
  await button('响应内容');
  assert.equal(await evaluate(`[...document.querySelectorAll('.code-block pre')].at(-1).textContent`), log.response_body);
  await bodyAction('response', '读取完整正文'); await waitText('RESPONSE-END-MARKER');
  assert.equal(await bodyText('response'), fullBodies.response.toString());
  await bodyAction('response', '下载原始文件'); await checkDownload(log.id, 'response', fullBodies.response);
  await button('复制元数据');
  await waitText('已复制');
  assert.deepEqual(JSON.parse(await evaluate('navigator.clipboard.readText()')), log);
  await button('导出元数据');
  const filename = path.join(downloads, 'request-browser-contract-log-metadata.json');
  await waitFor(async () => { try { return !!await readFile(filename); } catch { return false; } }, 'JSON download');
  assert.deepEqual(JSON.parse(await readFile(filename, 'utf8')), log);
  await screenshot('log-detail-mobile', 375, 1000);
  await command('Input.dispatchKeyEvent', { type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
  await command('Input.dispatchKeyEvent', { type: 'keyUp', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27 });
  await waitFor(() => evaluate('document.querySelectorAll("dialog[open]").length === 0'), 'Escape closes detail');
  await command('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1080, deviceScaleFactor: 1, mobile: false });
  entries = [partialLog, binaryLog, log];
  await field('搜索日志', ''); await button('刷新'); await sleep(100);
  await clickExpression(`document.querySelector('[aria-label="查看请求 partial-capture"]')`);
  await button('响应内容'); await waitText('实际捕获不完整：capture limit reached');
  bodyMode = 'missing';
  await bodyAction('response', '读取已捕获正文'); await waitText('正文不可用（404）');
  bodyMode = '';
  await bodyAction('response', '读取已捕获正文'); await waitText('partial capture bytes');
  await bodyAction('response', '下载原始文件'); await checkDownload(partialLog.id, 'response', partialBytes);
  await clickExpression(`document.querySelector('dialog [aria-label="关闭对话框"]')`);
  await clickExpression(`document.querySelector('[aria-label="查看请求 binary-capture"]')`);
  await button('响应内容'); await bodyAction('response', '读取完整正文');
  await waitText('不转换为文本显示');
  await bodyAction('response', '下载原始文件'); await checkDownload(binaryLog.id, 'response', compressedBytes);
  await clickExpression(`document.querySelector('dialog [aria-label="关闭对话框"]')`);
  await clickExpression(`document.querySelector('[aria-label="查看请求 browser-contract-log"]')`);
  await button('响应内容'); bodyMode = 'slow'; await bodyAction('response', '读取完整正文');
  await waitText('正在读取…');
  await clickExpression(`document.querySelector('dialog [aria-label="关闭对话框"]')`);
  await waitFor(() => abortedBodies > 0, 'closing the detail aborts its raw body request'); bodyMode = '';
  await button('清空日志'); await button('确认清空');
  await waitText('尚无请求记录'); assert.equal(cleared, true);

  await nav('网关设置');
  await waitText('0 = 不限制磁盘捕获长度');
  await button('清除令牌');
  await field('保留条数', '49');
  auth = 401;
  await nav('请求日志'); await button('刷新'); await waitText('HTTP 401');
  await button('填写令牌');
  await field('管理员令牌', 'browser-session-token');
  await button('应用会话令牌');
  await button('完成');
  await waitText('有未保存的配置修改');
  await nav('网关设置');
  assert.equal(await evaluate(`document.getElementById([...document.querySelectorAll('label')].find(e=>e.textContent==='保留条数').htmlFor).value`), '49');
  assert.equal(await evaluate('sessionStorage.getItem("agent-relay.admin-token")'), 'browser-session-token');
  assert.equal(await evaluate('localStorage.length'), 0);
  assert.ok(requests.some(r => r.authorization === 'Bearer browser-session-token'));
  await screenshot('settings-desktop');
  auth = 403;
  await button('清除令牌');
  await nav('请求日志'); await button('刷新'); await waitText('HTTP 403');
  await waitText('loopback');
  await screenshot('auth-error');
  auth = 0;
  await button('放弃修改'); await button('放弃修改');
  await waitText('配置已同步');

  await nav('供应商管理');
  await clickExpression(`document.querySelector('[aria-label="删除供应商 new-upstream"]')`);
  await button('从草稿中删除');
  assert.equal(await evaluate('document.querySelectorAll(".upstream-card").length'), 1);
  await button('放弃修改'); await button('放弃修改');
  assert.equal(await evaluate('document.querySelectorAll(".upstream-card").length'), 2);
  await button('编辑');
  await field('供应商名称', 'discard-local-edit');
  await clickExpression(`document.querySelector('dialog[open] [aria-label="关闭对话框"]')`);
  await button('放弃编辑');
  assert.equal(await evaluate('document.querySelectorAll("dialog[open]").length'), 0);
  assert.notEqual(await evaluate('getComputedStyle(document.body).overflow'), 'hidden');
  await waitText('preserved-upstream');

  configError = true;
  await command('Page.reload');
  await waitText('无法加载配置');
  await nav('供应商管理');
  await waitText('配置尚不可用');
  assert.ok(await evaluate(`[...document.querySelectorAll('button')].find(e=>e.textContent.includes('保存配置')).disabled`));
  assert.equal(puts.length, 1);
  configError = false;
  await button('重试加载');
  await waitText('配置已同步');
  await nav('网关设置');
  await field('保留条数', '49');
  await clickExpression(`document.querySelector('.advanced-settings summary')`);
  await field('正文嗅探时限（ms）', '0'); await field('错误正文读取时限（ms）', '60001');
  await button('保存配置');
  await waitText('正文嗅探时限必须是 1–60000 ms'); await waitText('错误正文读取时限必须是 1–60000 ms');
  assert.equal(puts.length, 1, 'invalid timeouts must not be clamped or submitted');
  await field('正文嗅探时限（ms）', '500'); await field('错误正文读取时限（ms）', '2500');
  rejectSave = true;
  await button('保存配置');
  await waitText('server rejected configuration');
  await waitText('草稿已保留');
  await waitText('有未保存的配置修改');
  rejectSave = false;
  await button('保存配置');
  await waitText('配置已保存');
  assert.equal(puts.length, 2);
  assert.equal(puts[1].log_limit, 49);
  assert.equal(puts[1].body_sniff_timeout_ms, 500);
  assert.equal(puts[1].error_body_timeout_ms, 2500);
  assert.deepEqual(puts[1].future_security, { preserve: true });

  assert.equal(await evaluate('location.pathname'), '/');
  assert.ok(resourceURLs.every(url => !url.includes(':8080')));
  assert.deepEqual(pageErrors, []);
  assert.ok((await readdir(results)).some(file => file.endsWith('.png')));
  console.log('PASS browser contract: retained original flows + strict limits/masks; no eager bodies; >32KiB end markers; preview vs real partial capture; raw 404/auth/abort; attempt headers; byte-exact text and gzip downloads; metadata labeling; responsive 375/768/1024/1440; no page exceptions.');
  console.log(`Screenshots: ${results}`);
} catch (error) {
  if (browser) {
    try {
      console.error('Visible page text:', await browser.evaluate('document.body.innerText.slice(0,18000)'));
      await browser.screenshot('failure');
    } catch { /* keep original failure */ }
  }
  throw error;
} finally {
  await browser?.close();
  server.closeAllConnections();
  await new Promise(resolve => server.close(resolve));
}
