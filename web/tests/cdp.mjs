// Shared native-CDP harness for fixture and real-Go UI tests. No app/test dependency.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import path from 'node:path';

export const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
export async function waitFor(check, description, timeoutMS = 12000) {
  // Host wall-clock corrections must not shorten a polling assertion's budget.
  const until = performance.now() + timeoutMS;
  while (performance.now() < until) { if (await check()) return; await sleep(80); }
  throw new Error(`Timed out: ${description}`);
}

export async function stopProcess(child) {
  if (!child?.pid || child.exitCode !== null || child.signalCode !== null) return;
  const exited = new Promise(resolve => child.once('exit', resolve));
  child.kill('SIGTERM');
  const timer = setTimeout(() => child.kill('SIGKILL'), 5000);
  await exited;
  clearTimeout(timer);
}

export async function launchBrowser({ origin, results, profileParent = results }) {
  const chromePath = process.env.CHROME_PATH;
  assert.ok(chromePath, 'Set CHROME_PATH to an existing Chromium/Chrome executable.');
  await mkdir(results, { recursive: true });
  await mkdir(profileParent, { recursive: true });
  const profile = await mkdtemp(path.join(profileParent, 'chrome-profile-'));
  const downloadParent = path.join(results, 'downloads');
  await mkdir(downloadParent, { recursive: true });
  const downloads = await mkdtemp(path.join(downloadParent, 'run-'));
  const browser = spawn(chromePath, ['--headless=new', '--no-sandbox', '--disable-dev-shm-usage', '--disable-background-timer-throttling', '--disable-renderer-backgrounding', '--no-first-run', '--remote-debugging-port=0', `--user-data-dir=${profile}`, 'about:blank'], { stdio: ['ignore', 'ignore', 'pipe'] });
  let ws;
  let session;
  let serial = 0;
  const pending = new Map();
  const pageErrors = [];
  const resourceURLs = [];
  const apiResponses = [];
  const send = (method, params = {}, sessionId) => new Promise((resolve, reject) => {
    const id = ++serial;
    const timer = setTimeout(() => { pending.delete(id); reject(new Error(`CDP timeout: ${method}`)); }, 15000);
    pending.set(id, { resolve, reject, timer });
    ws.send(JSON.stringify({ id, method, params, ...(sessionId ? { sessionId } : {}) }));
  });
  const command = (method, params = {}) => send(method, params, session);
  const evaluate = async expression => {
    const result = await command('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
    if (result.exceptionDetails) throw new Error(result.exceptionDetails.exception?.description ?? result.exceptionDetails.text);
    return result.result.value;
  };
  const waitText = text => waitFor(() => evaluate(`document.body.innerText.includes(${JSON.stringify(text)})`), text);
  async function clickExpression(expression) {
    const rect = await evaluate(`(() => { const e = ${expression}; if (!e) throw Error('Missing click target'); e.scrollIntoView({block:'center'}); const r=e.getBoundingClientRect(); return {x:r.x+r.width/2,y:r.y+r.height/2}; })()`);
    await command('Input.dispatchMouseEvent', { type: 'mousePressed', button: 'left', clickCount: 1, ...rect });
    await command('Input.dispatchMouseEvent', { type: 'mouseReleased', button: 'left', clickCount: 1, ...rect });
    await sleep(90);
  }
  const button = text => clickExpression(`(() => { const scope=[...document.querySelectorAll('dialog[open]')].at(-1)??document; return [...scope.querySelectorAll('button')].find(e=>e.textContent.trim()===${JSON.stringify(text)} && e.getClientRects().length); })()`);
  const nav = text => clickExpression(`[...document.querySelectorAll('.nav-item')].find(e=>e.textContent.includes(${JSON.stringify(text)}))`);
  const fieldElement = label => `(() => { const scope=[...document.querySelectorAll('dialog[open]')].at(-1)??document; const l=[...scope.querySelectorAll('label')].find(e=>e.textContent.trim()===${JSON.stringify(label)}); if(!l) throw Error('Missing field'); return document.getElementById(l.htmlFor); })()`;
  const fieldValue = label => evaluate(`(${fieldElement(label)}).value`);
  async function field(label, value) {
    await evaluate(`(() => { const e=${fieldElement(label)}; e.focus(); const proto=e.tagName==='TEXTAREA'?HTMLTextAreaElement.prototype:HTMLInputElement.prototype; Object.getOwnPropertyDescriptor(proto,'value').set.call(e,${JSON.stringify(value)}); e.dispatchEvent(new Event('input',{bubbles:true})); e.dispatchEvent(new Event('change',{bubbles:true})); })()`);
    await sleep(60);
  }
  async function screenshot(name, width = 1440, height = 1080) {
    await command('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: 1, mobile: false });
    await evaluate('window.scrollTo(0,0)');
    await sleep(160);
    await evaluate('document.fonts.ready.then(()=>true)');
    const image = await command('Page.captureScreenshot', { format: 'png', captureBeyondViewport: false });
    await writeFile(path.join(results, `${name}.png`), Buffer.from(image.data, 'base64'));
    const noOverflow = await evaluate('document.documentElement.scrollWidth <= innerWidth');
    if (!noOverflow) console.error('Overflow diagnostics:', await evaluate(`JSON.stringify({width:innerWidth,doc:document.documentElement.scrollWidth,nodes:[...document.querySelectorAll('body *')].map(e=>({tag:e.tagName,class:e.className,x:e.getBoundingClientRect().x,right:e.getBoundingClientRect().right})).filter(r=>r.right>innerWidth+1 || r.x < -1).slice(0,24)})`));
    assert.ok(noOverflow, `No horizontal overflow at ${width}px (${name})`);
  }
  async function close() {
    if (ws?.readyState === WebSocket.OPEN) { try { await send('Browser.close'); } catch {} ws.close(); }
    await stopProcess(browser);
    for (const item of pending.values()) clearTimeout(item.timer);
    pending.clear();
    await rm(profile, { recursive: true, force: true });
  }

  try {
    const endpoint = await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('Chromium did not expose a CDP endpoint')), 15000);
      browser.stderr.on('data', chunk => { const match = chunk.toString().match(/DevTools listening on (ws:\/\/\S+)/); if (match) { clearTimeout(timer); resolve(match[1]); } });
      browser.once('error', error => { clearTimeout(timer); reject(error); });
      browser.once('exit', code => { clearTimeout(timer); if (code) reject(new Error(`Chromium exit ${code}`)); });
    });
    ws = new WebSocket(endpoint);
    await new Promise((resolve, reject) => { ws.addEventListener('open', resolve, { once: true }); ws.addEventListener('error', reject, { once: true }); });
    ws.addEventListener('message', event => {
      const value = JSON.parse(event.data);
      if (value.id && pending.has(value.id)) { const { resolve, reject, timer } = pending.get(value.id); clearTimeout(timer); pending.delete(value.id); value.error ? reject(new Error(JSON.stringify(value.error))) : resolve(value.result); }
      if (value.method === 'Runtime.exceptionThrown') pageErrors.push(value.params.exceptionDetails);
      if (value.method === 'Network.requestWillBeSent') resourceURLs.push(value.params.request.url);
      if (value.method === 'Network.responseReceived' && value.params.response.url.startsWith(`${origin}/api/`)) apiResponses.push({ url: value.params.response.url, status: value.params.response.status, time: value.params.timestamp });
      if (value.method === 'Network.responseReceived' && value.params.response.url.startsWith(`${origin}/assets/`) && value.params.response.status >= 400) pageErrors.push({ asset: value.params.response.url, status: value.params.response.status });
    });
    const target = await send('Target.createTarget', { url: 'about:blank' });
    session = (await send('Target.attachToTarget', { targetId: target.targetId, flatten: true })).sessionId;
    await command('Runtime.enable'); await command('Page.enable'); await command('Network.enable');
    await command('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1080, deviceScaleFactor: 1, mobile: false });
    await send('Browser.setDownloadBehavior', { behavior: 'allow', downloadPath: downloads });
    await send('Browser.grantPermissions', { origin, permissions: ['clipboardReadWrite', 'clipboardSanitizedWrite'] });
    await command('Page.navigate', { url: origin });
    await send('Target.activateTarget', { targetId: target.targetId });
    await command('Page.bringToFront');
    return { command, evaluate, waitText, clickExpression, button, nav, field, fieldValue, screenshot, close, downloads, pageErrors, resourceURLs, apiResponses };
  } catch (error) { await close(); throw error; }
}
