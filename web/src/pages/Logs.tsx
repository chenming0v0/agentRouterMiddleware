import { ArrowDownToLine, ArrowUpRight, FileClock, Filter, RefreshCw, Search, ShieldAlert, Trash2 } from 'lucide-react';
import { useMemo, useState } from 'react';
import { api, errorMessage } from '../api';
import { downloadJSON, duration, time } from '../lib';
import type { LogEntry } from '../types';
import { Btn, CheckField, Confirm, Empty, Field, Notice, Panel, Status } from '../components/ui';

export function Logs({ logs, total, loading, error, updated, live, onLive, onRefresh, onSelect, onCleared }: {
  logs: LogEntry[]; total: number; loading: boolean; error: string; updated: Date | null; live: boolean;
  onLive: (value: boolean) => void; onRefresh: () => void; onSelect: (log: LogEntry) => void; onCleared: () => void;
}) {
  const [query, setQuery] = useState('');
  const [status, setStatus] = useState('all');
  const [method, setMethod] = useState('all');
  const [retry, setRetry] = useState(false);
  const [confirmClear, setConfirmClear] = useState(false);
  const [clearing, setClearing] = useState(false);
  const [clearError, setClearError] = useState('');
  const methods = [...new Set(logs.map(log => log.method))].sort();
  const filtered = useMemo(() => logs.filter(log => {
    if (status === 'network' && log.status !== 0) return false;
    if (status === 'success' && !(log.status >= 200 && log.status < 300)) return false;
    if (status === 'redirect' && !(log.status >= 300 && log.status < 400)) return false;
    if (status === 'client' && !(log.status >= 400 && log.status < 500)) return false;
    if (status === 'server' && log.status < 500) return false;
    if (method !== 'all' && log.method !== method) return false;
    if (retry && log.attempt_count <= 1) return false;
    const needle = query.trim().toLowerCase();
    return !needle || [log.id, log.path, log.query, log.model, log.upstream_name, log.upstream_id, log.client_ip, String(log.status)].some(value => (value || '').toLowerCase().includes(needle));
  }), [logs, status, method, retry, query]);

  async function clear() {
    setClearing(true); setClearError('');
    try { await api('/logs', { method: 'DELETE' }); setConfirmClear(false); onCleared(); }
    catch (e) { setClearError(errorMessage(e)); }
    finally { setClearing(false); }
  }
  function resetFilters() { setQuery(''); setStatus('all'); setMethod('all'); setRetry(false); }
  return <div className="page-stack">
    <div className="page-heading"><div><div className="eyebrow">REQUEST INSPECTOR</div><h1>请求日志</h1><p>从一次异常响应，追溯每次上游尝试。</p></div><div className="heading-actions"><CheckField checked={live} onChange={onLive} className="live-check">自动刷新</CheckField><Btn variant="secondary" busy={loading} onPress={onRefresh}><RefreshCw size={15} />刷新</Btn></div></div>
    <div className="logs-info"><div><span className={`health-dot ${live ? '' : 'dot-muted'}`} /><span>{live ? '每 3 秒自动获取' : '自动刷新已暂停'}</span><span className="separator-dot">·</span><span>默认读取最新 50 条</span></div><span className="mono">{updated ? `更新于 ${time(updated.toISOString())}` : '尚未更新'}</span></div>
    {error && <Notice tone="error">{error}{logs.length > 0 && ' 当前保留上次成功加载的记录。'}</Notice>}
    <Panel className="logs-panel">
      <div className="log-filters"><div className="search-field"><Search size={16} /><Field label="搜索日志" className="sr-label" value={query} placeholder="搜索路径、模型、上游或请求 ID…" onChange={e => setQuery(e.target.value)} /></div><div className="select-field"><label htmlFor="status-filter" className="sr-only">状态筛选</label><select id="status-filter" value={status} onChange={e => setStatus(e.target.value)}><option value="all">全部状态</option><option value="success">2xx 成功</option><option value="redirect">3xx 重定向</option><option value="client">4xx 客户端错误</option><option value="server">5xx 服务端错误</option><option value="network">网络错误</option></select></div><div className="select-field"><label htmlFor="method-filter" className="sr-only">方法筛选</label><select id="method-filter" value={method} onChange={e => setMethod(e.target.value)}><option value="all">全部方法</option>{methods.map(m => <option key={m} value={m}>{m}</option>)}</select></div><CheckField checked={retry} onChange={setRetry}>仅重试</CheckField></div>
      <div className="log-result-bar"><span><Filter size={13} />{filtered.length} 条匹配 <span className="muted">/ 已加载 {logs.length} 条</span></span><div><Btn variant="ghost" size="sm" isDisabled={!filtered.length} onPress={() => downloadJSON(filtered, `agent-relay-metadata-${Date.now()}.json`)}><ArrowDownToLine size={14} />导出元数据</Btn><Btn variant="ghost" size="sm" isDisabled={clearing || total === 0} onPress={() => { setConfirmClear(true); setClearError(''); }}><Trash2 size={14} />清空日志</Btn></div></div>
      {filtered.length ? <div className="table-scroll"><table className="request-table full-log-table"><thead><tr><th>时间 / 路径</th><th>模型</th><th>上游</th><th>状态</th><th>尝试</th><th>耗时</th><th><span className="sr-only">详情</span></th></tr></thead><tbody>{filtered.map(log => <tr key={log.id}><td><button className="table-link" onClick={() => onSelect(log)}><span className="mono muted small">{time(log.time)} <b className="method">{log.method}</b></span><strong className="mono truncate" title={log.path}>{log.path}</strong></button></td><td><span className="mono cell-truncate" title={log.model}>{log.model || '—'}</span></td><td><span className="cell-truncate" title={log.upstream_name}>{log.upstream_name || '—'}</span></td><td><Status code={log.status} /></td><td><span className={`attempt-chip ${log.attempt_count > 1 ? 'has-retry' : ''}`}>{log.attempt_count > 1 && <RefreshCw size={11} />}{log.attempt_count} 次</span></td><td className="mono nowrap">{duration(log.duration_ms)}</td><td><Btn variant="ghost" isIconOnly size="sm" aria-label={`查看请求 ${log.id}`} onPress={() => onSelect(log)}><ArrowUpRight size={16} /></Btn></td></tr>)}</tbody></table></div> : <Empty icon={<FileClock size={30} />} title={loading && !updated ? '正在读取日志' : logs.length ? '没有符合筛选条件的请求' : error ? '暂时无法读取日志' : '尚无请求记录'} action={logs.length ? <Btn variant="secondary" onPress={resetFilters}>重置筛选</Btn> : undefined}>{logs.length ? '尝试调整状态、方法或搜索关键词。筛选仅作用于已加载的最新记录。' : '请求经过网关后会自动记录。这里不包含管理界面的 API 请求。'}</Empty>}
      <div className="panel-foot spread"><span>服务端保留 {total} 条 · 当前最多读取 50 条</span><span className="mono">NEWEST FIRST</span></div>
    </Panel>
    <p className="page-note"><ShieldAlert size={15} />时间列为请求开始时间。元数据只含正文预览，完整存档请在详情中按需读取。请求头由后端脱敏，正文仍可能包含密钥或敏感提示词，分享前请检查。</p>
    {confirmClear && <Confirm title="清空所有请求日志？" label="确认清空" danger busy={clearing} onClose={() => setConfirmClear(false)} onConfirm={() => void clear()}><p>将删除服务端保留的所有请求日志，而不仅是当前筛选结果。此操作不可撤销。</p><p className="muted small">统计计数是否重置由当前后端实现决定，操作后会重新获取统计。</p>{clearError && <Notice tone="error">{clearError}</Notice>}</Confirm>}
  </div>;
}
