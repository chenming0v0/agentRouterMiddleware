import type { Config, Upstream } from './types.ts';

export const number = (value: number) => new Intl.NumberFormat('zh-CN').format(value);
export const duration = (ms: number) => ms < 1_000 ? `${number(ms)} ms` : `${(ms / 1_000).toFixed(2)} s`;
export function uptime(seconds: number) {
  const d = Math.floor(seconds / 86_400);
  const h = Math.floor(seconds % 86_400 / 3_600);
  const m = Math.floor(seconds % 3_600 / 60);
  return d ? `${d} 天 ${h} 小时` : h ? `${h} 小时 ${m} 分钟` : `${m} 分 ${Math.floor(seconds % 60)} 秒`;
}
export function time(value: string, full = false) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value || '—';
  return full ? date.toLocaleString('zh-CN', { hour12: false }) : date.toLocaleTimeString('zh-CN', { hour12: false });
}
export function bytes(value: number) {
  if (value === 0) return '不截断';
  return value >= 1_048_576 ? `${+(value / 1_048_576).toFixed(2)} MiB` : value >= 1_024 ? `${+(value / 1_024).toFixed(2)} KiB` : `${value} B`;
}
export const splitList = (value: string) => [...new Set(value.split(/[,，\n]/).map(x => x.trim()).filter(Boolean))];
export const validInteger = (n: number, min = 0) => Number.isSafeInteger(n) && n >= min;
export const MAX_BYTES = 1_073_741_824;
const boundedInteger = (n: number, min: number, max: number) => validInteger(n, min) && n <= max;
export const asNumber = (value: string) => value.trim() ? Number(value) : Number.NaN;
export const inputNumber = (value: number) => Number.isFinite(value) ? value : '';

// Dedicated provider URLs are /{id}/... Mirror of the Go backend: a single
// URL-safe path segment, first character alphanumeric, never a reserved prefix
// (case-insensitive) that would shadow management/static/legacy routes.
const UPSTREAM_ID_PATTERN = /^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/;
const RESERVED_SEGMENTS = new Set(['api', 'assets', 'v1', 'v1beta', 'v2']);

export function validUpstreamID(id: string) {
  return UPSTREAM_ID_PATTERN.test(id) && !RESERVED_SEGMENTS.has(id.toLowerCase());
}

export function upstreamIDError(id: string): string {
  if (!UPSTREAM_ID_PATTERN.test(id)) return '标识符必须以字母或数字开头，仅含字母、数字、- 或 _，长度 1–64。';
  return `标识符 "${id}" 是保留的路径前缀（api / assets / v1 / v1beta / v2，不区分大小写），请换一个。`;
}

// Slugify free-form text into a usable identifier; callers de-duplicate.
export function suggestID(name: string) {
  const slug = name.trim().toLowerCase().replace(/[^a-z0-9_-]+/g, '-').replace(/^[-_]+|[-_]+$/g, '').slice(0, 64);
  return /^[a-z0-9]/.test(slug) ? slug : '';
}

// URL-safe identifier for a new provider. crypto.randomUUID() is only exposed in
// a secure context, but this admin UI is reachable over plain HTTP on a LAN IP,
// so use getRandomValues() (available in insecure contexts) when present. These
// IDs select a route and are not secrets; with no Web Crypto at all, fall back to
// a timestamp + counter + Math.random value that is still unique per call.
let providerIDSequence = 0;
export function newProviderID(): string {
  const webcrypto = globalThis.crypto;
  if (webcrypto?.getRandomValues) {
    const bytes = new Uint8Array(12);
    webcrypto.getRandomValues(bytes);
    return `p_${[...bytes].map(byte => byte.toString(16).padStart(2, '0')).join('')}`;
  }
  const unique = `${Date.now().toString(16)}${(providerIDSequence++).toString(16)}${Math.floor(Math.random() * 0xffffffff).toString(16)}`;
  return `p_${unique.padEnd(24, '0').slice(0, 24)}`;
}

// Origin of the running gateway API. Vite's dev port is a static-file server
// that never forwards /{id}/... requests, so development points at the backend.
// import.meta.env is Vite build-time data; guard its non-Vite absence for tests.
const viteEnv = (import.meta as unknown as { env?: { DEV?: boolean } }).env;
export const publicOrigin = (viteEnv?.DEV && typeof window !== 'undefined')
  ? 'http://127.0.0.1:18851'
  : (globalThis.location?.origin ?? '');

// Pure, copyable provider base. No query strings, no credentials.
export const providerBaseURL = (id: string) => `${publicOrigin}/${encodeURIComponent(id)}`;

export function validURL(value: string) {
  try {
    const url = new URL(value);
    return ['http:', 'https:'].includes(url.protocol) && !!url.hostname && !url.username && !url.password && !value.includes('#');
  } catch { return false; }
}

export function parseHeaders(text: string): Record<string, string> {
  const parsed: unknown = JSON.parse(text || '{}');
  if (!parsed || Array.isArray(parsed) || typeof parsed !== 'object') throw new Error('请求头必须是 JSON 对象，例如 {"Authorization":"Bearer …"}。');
  for (const [key, value] of Object.entries(parsed)) {
    if (typeof value !== 'string') throw new Error(`请求头 ${key} 的值必须是字符串。`);
    if (!/^[!#$%&'*+.^_`|~0-9a-z-]+$/i.test(key)) throw new Error(`请求头名称无效：${key || '空名称'}`);
    if (/[\x00-\x08\x0a-\x1f\x7f]/.test(value)) throw new Error(`请求头 ${key} 的值不能包含换行或非法控制字符。`);
  }
  return parsed as Record<string, string>;
}

export function validateUpstream(upstream: Upstream): string[] {
  const errors: string[] = [];
  if (!upstream.id) errors.push('上游标识符不能为空。');
  else if (!validUpstreamID(upstream.id)) errors.push(upstreamIDError(upstream.id));
  if (!upstream.name.trim()) errors.push('请填写上游名称。');
  if (!validURL(upstream.base_url)) errors.push('上游地址必须是完整的 http:// 或 https:// URL，不能包含用户名、密码或片段。');
  if (!boundedInteger(upstream.weight, 0, 1_000_000)) errors.push('权重必须是 0–1000000 的整数。');
  if (!boundedInteger(upstream.timeout_ms, 1, 86_400_000)) errors.push('超时必须是 1–86400000 的整数（毫秒）。');
  return errors;
}

export function validateConfig(config: Config): string[] {
  const errors: string[] = [];
  if (!config.listen.trim()) errors.push('设置：监听地址不能为空。');
  if (!boundedInteger(config.log_limit, 1, 1000)) errors.push('设置：日志保留条数必须是 1–1000 的整数。');
  if (!boundedInteger(config.max_request_body_bytes, 1, MAX_BYTES)) errors.push('设置：请求体上限必须是 1–1073741824 字节的整数（最大 1 GiB）。');
  if (!boundedInteger(config.max_buffer_bytes, 1, MAX_BYTES)) errors.push('设置：缓冲区上限必须是 1–1073741824 字节的整数（最大 1 GiB）。');
  if (!boundedInteger(config.max_log_body_bytes, 0, MAX_BYTES)) errors.push('设置：日志正文上限必须是 0–1073741824 字节的整数；0 表示不限制磁盘捕获长度。');
  if (config.body_sniff_timeout_ms !== undefined && !boundedInteger(config.body_sniff_timeout_ms, 1, 60_000)) errors.push('设置：正文嗅探时限必须是 1–60000 ms 的整数。');
  if (config.error_body_timeout_ms !== undefined && !boundedInteger(config.error_body_timeout_ms, 1, 60_000)) errors.push('设置：错误正文读取时限必须是 1–60000 ms 的整数。');
  const r = config.retry;
  if (!boundedInteger(r.max_attempts, 1, 20)) errors.push('重试规则：总尝试次数必须是 1–20 的整数。');
  if (!boundedInteger(r.base_delay_ms, 0, 3_600_000)) errors.push('重试规则：初始延迟必须是 0–3600000 ms 的整数。');
  if (!boundedInteger(r.max_delay_ms, 0, 3_600_000) || r.max_delay_ms < r.base_delay_ms) errors.push('重试规则：延迟上限必须是 0–3600000 ms 的整数，且不能小于初始延迟。');
  if ((r.status_codes ?? []).some(code => !validInteger(code, 100) || code > 599)) errors.push('重试规则：HTTP 状态码必须是 100–599 的整数。');
  if ((r.body_regexes ?? []).some(pattern => !pattern.trim())) errors.push('重试规则：正则表达式不能为空，请填写或删除空规则。');
  const ids = new Set<string>();
  const names = new Set<string>();
  for (const u of config.upstreams ?? []) {
    errors.push(...validateUpstream(u).map(e => `上游 ${u.name || u.id}：${e}`));
    if (ids.has(u.id)) errors.push(`上游 ID 重复：${u.id}`);
    ids.add(u.id);
    if (names.has(u.name)) errors.push(`上游名称重复：${u.name}`);
    names.add(u.name);
  }
  // RE2 syntax is deliberately validated by the Go API, never JavaScript RegExp.
  return errors;
}

export function configShape(value: unknown): value is Config {
  if (!value || typeof value !== 'object') return false;
  const c = value as Config;
  return typeof c.listen === 'string' && !!c.retry && typeof c.retry === 'object' &&
    typeof c.retry.max_attempts === 'number' && (c.upstreams == null || Array.isArray(c.upstreams)) &&
    ['log_limit', 'max_request_body_bytes', 'max_buffer_bytes', 'max_log_body_bytes'].every(key => typeof c[key] === 'number');
}

export function downloadJSON(value: unknown, filename: string) {
  downloadBlob(new Blob([JSON.stringify(value, null, 2)], { type: 'application/json;charset=utf-8' }), filename);
}

export function downloadBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  (document.querySelector('dialog[open]') ?? document.body).appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 60_000);
}
