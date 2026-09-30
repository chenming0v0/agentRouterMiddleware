import { ArrowDownToLine, ArrowRight, Clock3, FileJson, GitBranch, ShieldAlert } from 'lucide-react';
import { useEffect, useState } from 'react';
import { api, ApiError, errorMessage } from '../api';
import { captureInfo, captureState } from '../body';
import { downloadJSON, duration, time } from '../lib';
import type { LogEntry } from '../types';
import { LogBody } from '../components/LogBody';
import { Btn, CodeBlock, CopyButton, Dialog, Notice, Status } from '../components/ui';

function headersText(headers: Record<string, string[]> | null | undefined) {
  return Object.entries(headers ?? {}).flatMap(([key, values]) => (values ?? []).map(value => `${key}: ${value}`)).join('\n');
}

export function LogDetail({ log: initial, onClose }: { log: LogEntry; onClose: () => void }) {
  const [log, setLog] = useState(initial);
  const [metadataError, setMetadataError] = useState('');
  const [metadataLoading, setMetadataLoading] = useState(true);
  const [tab, setTab] = useState<'attempts' | 'request' | 'response'>('attempts');
  useEffect(() => {
    const controller = new AbortController();
    void api<LogEntry>(`/logs/${encodeURIComponent(initial.id)}`, { signal: controller.signal })
      .then(value => { if (!controller.signal.aborted) setLog(value); })
      .catch(e => {
        if (!controller.signal.aborted) setMetadataError(e instanceof ApiError && e.status === 404
          ? '记录已被淘汰或清空（404）。当前只保留已加载的元数据，存档正文可能无法读取。' : errorMessage(e));
      }).finally(() => { if (!controller.signal.aborted) setMetadataLoading(false); });
    return () => controller.abort();
  }, [initial.id]);
  const attempts = log.attempts ?? [];
  const captures = [captureInfo(log, 'request'), captureInfo(log, 'response'), ...attempts.map(attempt => captureInfo(attempt, 'response'))];
  const partial = captures.some(info => captureState(info) === 'partial');
  const shortened = log.request_body_truncated || log.response_body_truncated || attempts.some(a => a.response_body_truncated);
  const requestTarget = `${log.path}${log.query ? `?${log.query}` : ''}`;
  return <Dialog title="请求详情" eyebrow="REQUEST DETAIL" onClose={onClose} drawer footer={<>
    <span className="footer-hint"><ShieldAlert size={13} />元数据仅含正文预览；原始正文请逐项下载</span>
    <CopyButton text={JSON.stringify(log, null, 2)} label="复制元数据" />
    <Btn variant="secondary" onPress={() => downloadJSON(log, `request-${log.id.replace(/[^a-z0-9_-]/gi, '_')}-metadata.json`)}><ArrowDownToLine size={15} />导出元数据</Btn>
  </>}>
    <div className="detail-summary"><div className="request-target"><span className="method">{log.method}</span><code>{requestTarget}</code><Status code={log.status} /></div><div className="request-id"><span className="mono">{log.id}</span><CopyButton text={log.id} label="复制请求 ID" compact /></div>
      <dl className="detail-meta"><div><dt>请求时间</dt><dd>{time(log.time, true)}</dd></div><div><dt>总耗时</dt><dd className="mono">{duration(log.duration_ms)}</dd></div><div><dt>模型</dt><dd className="mono">{log.model || '未指定'}</dd></div><div><dt>客户端</dt><dd className="mono">{log.client_ip || '—'}</dd></div><div><dt>最终上游</dt><dd>{log.upstream_name || '未选中上游'}</dd></div><div><dt>尝试次数</dt><dd>{log.attempt_count} 次{log.retried ? ' · 已重试' : ''}</dd></div></dl>
    </div>
    {metadataLoading && <p className="field-hint">正在核对最新元数据，不会自动读取正文文件。</p>}
    {metadataError && <Notice tone="error">{metadataError}</Notice>}
    {partial ? <Notice>存在实际未完整捕获的正文。请查看各正文卡片的大小、错误及可用状态；下载只包含已保存部分。</Notice>
      : shortened && <Notice tone="info">列表正文为预览，省略预览不代表磁盘存档不完整。请按需读取正文，并核对各项捕获状态。</Notice>}
    {log.incomplete && <Notice>请求未完整结束，可能发生取消、连接中断或读取失败；HTTP 状态码不代表响应完整送达。</Notice>}
    {log.error && <Notice tone="error">请求错误：{log.error}</Notice>}
    {log.streaming && <Notice tone="info">此请求为流式响应。成功的 SSE 会立即透传，不逐帧执行正文正则，也不会在响应开始后重试。</Notice>}
    {log.regex_skipped && <Notice tone="info">此请求跳过了正文正则检查，可能是成功 SSE、压缩正文或嗅探超时 / 超过缓冲区上限。</Notice>}
    <div className="detail-tabs" role="tablist" aria-label="请求详情内容">{([{ id: 'attempts', label: '尝试链路', icon: GitBranch }, { id: 'request', label: '请求内容', icon: FileJson }, { id: 'response', label: '响应内容', icon: ArrowRight }] as const).map(item => <button key={item.id} role="tab" id={`detail-tab-${item.id}`} aria-controls="detail-tabpanel" aria-selected={tab === item.id} tabIndex={tab === item.id ? 0 : -1} className={tab === item.id ? 'selected' : ''} onClick={() => setTab(item.id)} onKeyDown={event => {
      if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return;
      event.preventDefault();
      const ids = ['attempts', 'request', 'response'] as const;
      const next = ids[(ids.indexOf(tab) + (event.key === 'ArrowRight' ? 1 : 2)) % 3];
      setTab(next); document.getElementById(`detail-tab-${next}`)?.focus();
    }}><item.icon size={15} />{item.label}{item.id === 'attempts' && <span>{attempts.length}</span>}</button>)}</div>
    <div className="detail-tabpanel" id="detail-tabpanel" role="tabpanel" aria-labelledby={`detail-tab-${tab}`}>
      {tab === 'attempts' && <div className="attempt-timeline">{attempts.length ? attempts.map((attempt, i) => <article className="attempt" key={i}>
        <div className="attempt-marker mono">{i + 1}</div><div className="attempt-content"><div className="attempt-title"><h3>{attempt.upstream_name || attempt.upstream_id || '未知上游'}</h3><Status code={attempt.status} /><span className="mono muted"><Clock3 size={12} />{duration(attempt.duration_ms)}</span></div><p className="attempt-url mono">{attempt.url}</p>
          {attempt.error && <Notice tone="error">{attempt.error}</Notice>}
          <div className="attempt-reason"><GitBranch size={14} /><span>{attempt.retry_reason || (i < attempts.length - 1 ? '继续尝试（服务端未提供原因）' : '尝试结束')}</span></div>
          {attempt.regex_skipped && <p className="field-hint">此尝试跳过了正文正则检查。</p>}
          <div className="attempt-payloads"><CodeBlock title={`第 ${i + 1} 次响应头`} value={headersText(attempt.response_headers)} />
            <LogBody logID={log.id} part={`attempt-${i + 1}`} title={`第 ${i + 1} 次响应正文`} preview={attempt.response_body ?? ''} shortened={attempt.response_body_truncated} capture={captureInfo(attempt, 'response')} headers={attempt.response_headers} />
          </div>
        </div></article>) : <div className="quiet-empty">该记录没有上游尝试详情；请查看最终请求与响应内容。</div>}</div>}
      {tab === 'request' && <div className="form-stack"><CodeBlock title="请求头" value={headersText(log.request_headers)} /><LogBody key="request" logID={log.id} part="request" title="请求正文" preview={log.request_body ?? ''} shortened={log.request_body_truncated} capture={captureInfo(log, 'request')} headers={log.request_headers} /></div>}
      {tab === 'response' && <div className="form-stack"><CodeBlock title="响应头" value={headersText(log.response_headers)} />{log.response_body_part && <p className="field-hint">正文存档指向 <code>{log.response_body_part}</code>，response 接口读取最终上游尝试的正文。</p>}<LogBody key="response" logID={log.id} part="response" title="响应正文" preview={log.response_body ?? ''} shortened={log.response_body_truncated} capture={captureInfo(log, 'response')} headers={log.response_headers} /></div>}
    </div>
  </Dialog>;
}
