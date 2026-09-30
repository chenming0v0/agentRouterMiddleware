const TOKEN_KEY = 'agent-relay.admin-token';

export class ApiError extends Error {
  status: number;
  constructor(message: string, status: number) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

export function getToken(): string {
  try { return sessionStorage.getItem(TOKEN_KEY) ?? ''; }
  catch { return ''; }
}

export function storeToken(token: string) {
  if (token.trim()) sessionStorage.setItem(TOKEN_KEY, token.trim());
  else sessionStorage.removeItem(TOKEN_KEY);
}

async function request(path: string, init: RequestInit, accept: string, timeoutMS: number): Promise<Response> {
  const headers = new Headers(init.headers);
  headers.set('Accept', accept);
  if (init.body) headers.set('Content-Type', 'application/json');
  const token = getToken();
  if (token) headers.set('Authorization', `Bearer ${token}`);
  const timeout = AbortSignal.timeout(timeoutMS);
  const signal = init.signal ? AbortSignal.any([init.signal, timeout]) : timeout;
  const response = await fetch(`/api${path}`, {
    ...init, headers, signal, credentials: 'same-origin', cache: 'no-store',
  });
  if (!response.ok) {
    let data: unknown;
    try { data = JSON.parse(await response.text()); } catch { /* Some reverse proxies return HTML errors. */ }
    const serverMessage = data && typeof data === 'object' && 'error' in data ? String(data.error) : '';
    let message = serverMessage || `请求失败（HTTP ${response.status}）`;
    if (response.status === 401) message = '管理员令牌缺失或无效。请填写与服务端 AGENTROUTER_ADMIN_TOKEN 一致的令牌。';
    if (response.status === 403) message = '访问被拒绝。请使用同源管理页面；服务端未设置令牌时仅允许 loopback 本机访问。远程访问需设置 AGENTROUTER_ADMIN_TOKEN。';
    if ((response.status === 401 || response.status === 403) && token === getToken()) {
      window.dispatchEvent(new CustomEvent('relay:auth-error', { detail: { status: response.status, message } }));
    }
    throw new ApiError(message, response.status);
  }
  return response;
}

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const response = await request(path, init, 'application/json', 30_000);
  const text = await response.text();
  try { return (text ? JSON.parse(text) : {}) as T; }
  catch { throw new ApiError('接口没有返回 JSON，请检查 /api 路由是否连接到网关。', response.status); }
}

// Body endpoints are opaque bytes. Do not pass them through JSON or UTF-8 re-encoding.
export async function apiBlob(path: string, signal?: AbortSignal): Promise<Blob> {
  try {
    const response = await request(path, { signal }, 'application/octet-stream', 120_000);
    return await response.blob();
  } catch (error) {
    if (error instanceof ApiError && error.status === 404) {
      throw new ApiError('正文不可用（404）：日志可能已被淘汰、清空，或存档文件缺失。可刷新日志后重新检查。', 404);
    }
    throw error;
  }
}

export function errorMessage(error: unknown) {
  if (error instanceof Error) {
    if (error.name === 'TimeoutError') return '请求超时，请检查网关连接后重试。';
    if (error instanceof TypeError) return '无法连接网关。请检查服务是否运行、网络与 /api 代理配置。';
    return error.message;
  }
  return String(error);
}
