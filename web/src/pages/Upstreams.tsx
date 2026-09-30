import { ArrowRight, Check, Clock3, Code2, Globe2, KeyRound, Link2, Network, Pencil, Plus, Radio, Server, Trash2, X } from 'lucide-react';
import { useEffect, useRef, useState } from 'react';
import { api, errorMessage } from '../api';
import type { ConfigState } from '../hooks';
import { asNumber, duration, newProviderID, parseHeaders, providerBaseURL, splitList, suggestID, upstreamIDError, validUpstreamID, validURL, validateUpstream } from '../lib';
import type { Upstream } from '../types';
import { Area, Btn, CheckField, Confirm, CopyButton, Dialog, Empty, Field, Notice } from '../components/ui';

interface Connectivity { ok: boolean; status: number; latency_ms: number; error?: string }
function ConnectionTest({ url }: { url: string }) {
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<Connectivity | null>(null);
  const [error, setError] = useState('');
  const generation = useRef(0);
  useEffect(() => {
    generation.current++; setBusy(false); setResult(null); setError('');
    return () => { generation.current++; };
  }, [url]);
  async function test() {
    const current = ++generation.current;
    setBusy(true); setResult(null); setError('');
    try {
      const response = await api<Connectivity>('/upstreams/test', { method: 'POST', body: JSON.stringify({ base_url: url }) });
      if (current === generation.current) setResult(response);
    } catch (e) { if (current === generation.current) setError(errorMessage(e)); }
    finally { if (current === generation.current) setBusy(false); }
  }
  const reachable = result && result.status > 0;
  return <div className="connection-test"><Btn variant="secondary" size="sm" busy={busy} isDisabled={!validURL(url)} onPress={() => void test()}><Radio size={15} />测试连通性</Btn>
    {result && <span className={`connection-result ${reachable ? 'text-success' : 'text-error'}`} role="status">{reachable ? <Check size={14} /> : <X size={14} />}{reachable ? `可达 · HTTP ${result.status} · ${duration(result.latency_ms)}` : `连接失败：${result.error || '未收到 HTTP 响应'}`}</span>}
    {error && <span className="field-error" role="alert">{error}</span>}
  </div>;
}

// A provider's dedicated copyable URL block. Copying never opens the editor.
function ProviderAddress({ id, baseUrl }: { id: string; baseUrl: string }) {
  const base = providerBaseURL(id);
  const v1 = `${base}/v1`;
  return <div className="provider-address">
    <div className="provider-address-row">
      <Link2 size={13} /><span className="provider-address-label">调用地址</span>
      <code className="mono truncate" title={base}>{base}</code>
      <CopyButton text={base} label="复制地址" />
      <CopyButton text={v1} label="复制 /v1 地址" />
    </div>
    <div className="provider-strip mono"><ArrowRight size={11} /><span>{base}<b>/v1/chat/completions</b> → {baseUrl || '（上游地址）'}<b>/v1/chat/completions</b></span></div>
  </div>;
}

function UpstreamEditor({ initial, isNew, existing, onClose, onApply }: { initial: Upstream; isNew: boolean; existing: string[]; onClose: () => void; onApply: (upstream: Upstream) => void }) {
  const initialFields = {
    name: initial.name, id: initial.id, base_url: initial.base_url, weight: String(initial.weight), timeout_ms: String(initial.timeout_ms),
    enabled: initial.enabled, paths: (initial.path_globs ?? []).join('\n'), models: (initial.model_globs ?? []).join('\n'), headers: JSON.stringify(initial.headers ?? {}, null, 2),
  };
  const [fields, setFields] = useState(initialFields);
  const [idTouched, setIdTouched] = useState(false);
  const [errors, setErrors] = useState<string[]>([]);
  const [discard, setDiscard] = useState(false);
  const dirty = JSON.stringify(fields) !== JSON.stringify(initialFields);
  const id = fields.id.trim();
  const idValid = validUpstreamID(id);
  const idConflict = isNew && idValid && id.toLowerCase() !== initial.id.toLowerCase() && existing.some(x => x.toLowerCase() === id.toLowerCase());
  const idIssue = (!idTouched && !isNew) || idValid || !id ? '' : upstreamIDError(id);
  const set = (key: keyof typeof fields, value: string | boolean) => {
    setFields(current => {
      const next = { ...current, [key]: value };
      if (isNew && key === 'name' && !idTouched) next.id = suggestID(String(value)) || newProviderID();
      return next;
    });
    setErrors([]);
  };
  useEffect(() => {
    if (!dirty) return;
    const handler = (e: BeforeUnloadEvent) => { e.preventDefault(); e.returnValue = ''; };
    window.addEventListener('beforeunload', handler);
    return () => window.removeEventListener('beforeunload', handler);
  }, [dirty]);
  function apply() {
    try {
      const next: Upstream = {
        ...initial, name: fields.name.trim(), id, base_url: fields.base_url.trim(), weight: asNumber(fields.weight), timeout_ms: asNumber(fields.timeout_ms),
        enabled: fields.enabled, path_globs: splitList(fields.paths), model_globs: splitList(fields.models), headers: parseHeaders(fields.headers),
      };
      const issues = validateUpstream(next).filter(Boolean);
      if (idConflict) issues.push(`标识符 "${id}" 已被其他供应商占用。`);
      if (issues.length) { setErrors(issues); return; }
      onApply(next);
    } catch (e) { setErrors([errorMessage(e)]); }
  }
  const close = () => dirty ? setDiscard(true) : onClose();
  return <><Dialog wide title={isNew ? '添加供应商' : '编辑供应商'} eyebrow={isNew ? 'NEW PROVIDER' : initial.id} onClose={close} footer={<>
    <span className="footer-hint">应用后需点击全局「保存配置」，专用地址才会生效。</span><Btn variant="secondary" onPress={close}>取消</Btn><Btn onPress={apply}><Check size={16} />应用到草稿</Btn>
  </>}>
    <div className="form-stack">
      {errors.length > 0 && <Notice tone="error"><ul>{errors.map(e => <li key={e}>{e}</li>)}</ul></Notice>}
      <div className="form-grid"><Field label="供应商名称" placeholder="例如 OpenAI 官方" value={fields.name} onChange={e => set('name', e.target.value)} autoFocus autoComplete="off" /><div className="enabled-field"><CheckField checked={fields.enabled} onChange={v => set('enabled', v)} hint="保存后，专用地址与旧路由才可使用。">启用此供应商</CheckField></div></div>
      {isNew
        ? <Field label="调用标识符" placeholder="例如 openai" value={fields.id} onChange={e => { setIdTouched(true); set('id', e.target.value); }} mono autoComplete="off" spellCheck={false} error={idIssue || (idConflict ? `标识符 "${id}" 已被其他供应商占用。` : '')} hint={<>名称会默认生成；保存后用于专用地址 <code>…/{id || '标识符'}/…</code>，且与供应商永久绑定。字母、数字、-、_，1–64 位；api / assets / v1 / v1beta / v2 为保留前缀。</>} />
        : <div className="field"><label>调用标识符</label><div className="readonly-id mono">{initial.id}</div><p className="field-hint">标识符与专用地址永久绑定，不能修改。专用地址为 <code>…/{initial.id}/…</code>。</p></div>}
      <Field label="上游根地址" placeholder="https://api.openai.com" value={fields.base_url} onChange={e => set('base_url', e.target.value)} mono type="url" autoComplete="off" hint="使用服务根地址。调用方请求 {标识符}/v1/… 时，网关在此地址后追加 /v1/…。" />
      <div className="path-example"><Code2 size={15} /><div><span>专用地址示例</span><code>{providerBaseURL(idValid || !isNew ? (isNew ? id : initial.id) : '…') || '…'} <b>+</b> /v1/chat/completions</code><p>调用方使用<b>该供应商原本的 API Key</b>（Authorization / X-API-Key 原样转发），这里不生成业务密钥；后台管理令牌只用于访问本后台，与供应商 API Key 无关。</p></div></div>
      <div className="form-grid"><Field label="请求超时（ms）" type="number" min={1} max={86_400_000} step={1} value={fields.timeout_ms} onChange={e => set('timeout_ms', e.target.value)} hint="1–86400000 ms。300000 ms = 5 分钟，最长 24 小时。" /><div className="connectivity-section"><ConnectionTest url={fields.base_url} /><p className="field-hint">只检测地址连通性：包括 4xx 在内的 HTTP 响应都表示可达，不代表模型可用或鉴权成功。</p></div></div>
      <details className="panel advanced-settings"><summary><span><Network size={18} /><strong>旧入口兼容</strong></span><span className="muted small">权重 / 路径 / 模型 / 附加请求头</span></summary><div className="panel-body form-stack">
        <p className="field-hint">仅影响<b>不带</b>供应商标识符的旧入口（/v1/… 直接转发）。带 {`/{标识符}/`} 前缀的专用地址始终路由到本供应商，忽略下列规则与附加请求头。</p>
        <div className="form-grid"><Field label="路由权重" type="number" min={0} max={1_000_000} step={1} value={fields.weight} onChange={e => set('weight', e.target.value)} hint="0–1000000，仅在匹配的上游之间比较；0 始终不参与旧入口路由。" /><div /></div>
        <div className="form-grid"><Area label="路径通配规则" rows={3} placeholder={'/v1/*\n/v1/chat/completions'} value={fields.paths} onChange={e => set('paths', e.target.value)} hint="换行或逗号分隔。留空表示不限制路径。" /><Area label="模型通配规则" rows={3} placeholder={'gpt-*\nclaude-*'} value={fields.models} onChange={e => set('models', e.target.value)} hint="换行或逗号分隔。留空表示不限制模型。" /></div>
        <Area label={<><KeyRound size={14} />附加请求头（JSON）</>} rows={5} value={fields.headers} onChange={e => set('headers', e.target.value)} spellCheck={false} hint="仅旧入口生效。仅接受字符串键值。服务端返回的 *** 表示保留原值；输入新值会替换，删除键会清除该请求头。请勿在此存放主流程必需信息。" />
      </div></details>
    </div>
  </Dialog>{discard && <Confirm title="放弃此供应商的编辑？" label="放弃编辑" danger onClose={() => setDiscard(false)} onConfirm={onClose}>此对话框中的修改尚未应用到配置草稿。关闭后这些修改将丢失。</Confirm>}</>;
}

export function Upstreams({ state, openNew, onNewHandled }: { state: ConfigState; openNew: boolean; onNewHandled: () => void }) {
  const [editing, setEditing] = useState<{ upstream: Upstream; isNew: boolean } | null>(null);
  const [remove, setRemove] = useState<Upstream | null>(null);
  const draftUpstreams = state.draft?.upstreams ?? [];
  // Saved config decides dedicated-address availability; the draft never fakes one.
  const savedIDs = new Set((state.saved?.upstreams ?? []).map(u => u.id));
  function add() {
    setEditing({ isNew: true, upstream: { id: newProviderID(), name: '', base_url: '', enabled: true, weight: 100, timeout_ms: 300_000, path_globs: [], model_globs: [], headers: {} } });
  }
  useEffect(() => {
    if (!openNew) return;
    add(); onNewHandled();
  }, [openNew, onNewHandled]);
  function apply(upstream: Upstream) {
    state.update(c => ({ ...c, upstreams: editing?.isNew ? [...(c.upstreams ?? []), upstream] : (c.upstreams ?? []).map(u => u.id === upstream.id ? upstream : u) }));
    setEditing(null);
  }
  return <div className="page-stack">
    <div className="page-heading"><div><div className="eyebrow">PROVIDER ENDPOINTS</div><h1>供应商管理<span className="heading-count">{draftUpstreams.length}</span></h1><p>每个供应商对应一个固定专用地址。调用方使用供应商原本的 API Key 直接访问。</p></div><Btn onPress={add} isDisabled={state.saving}><Plus size={17} />添加供应商</Btn></div>
    <div className="section-explainer"><Globe2 size={17} /><span>创建供应商 <ArrowRight size={13} /> 复制 <b>/标识符</b> 专用地址 <ArrowRight size={13} /> 调用方使用<b>供应商原本的 API Key</b> 请求 <b>…/标识符/v1/…</b> <ArrowRight size={13} /> 重试期间始终固定同一供应商</span></div>
    {draftUpstreams.length ? <div className="upstreams-grid">{draftUpstreams.map(u => {
      const routeID = validUpstreamID(u.id) ? u.id : null;
      const isSaved = savedIDs.has(u.id);
      return <article key={u.id} className={`upstream-card ${!u.enabled ? 'upstream-disabled' : ''}`}>
      <div className="upstream-card-head"><div className="provider-icon"><Server size={21} /></div><div className="provider-title"><h2>{u.name}</h2><span className="mono small muted">{u.id}</span></div><span className={`state-tag ${u.enabled ? 'state-enabled' : ''}`}>{u.enabled ? '已启用' : '已停用'}</span></div>
      {routeID && isSaved && <ProviderAddress id={routeID} baseUrl={u.base_url} />}
      {!isSaved && routeID && <div className="provider-pending"><Link2 size={13} /><code className="mono muted truncate">{providerBaseURL(routeID)}</code><span className="state-tag">保存后生效</span></div>}
      {!routeID && <div className="provider-pending"><span className="field-error">标识符 {u.id} 不是可用的专用地址，编辑后另存无法生效，请删除并以有效标识符重建。</span></div>}
      <div className="upstream-url mono"><Globe2 size={14} /><span>{u.base_url}</span></div>
      <div className="upstream-meta"><div><span><Clock3 size={12} />请求超时</span><strong className="mono">{duration(u.timeout_ms)}</strong></div><div><span>旧入口权重</span><strong className="mono">{u.weight}</strong></div><div><span>专用地址</span><strong>{isSaved && validUpstreamID(u.id) ? <code>/{u.id}/…</code> : '—'}</strong></div></div>
      <div className="upstream-card-actions"><CheckField checked={u.enabled} onChange={enabled => state.update(c => ({ ...c, upstreams: (c.upstreams ?? []).map(item => item.id === u.id ? { ...item, enabled } : item) }))} disabled={state.saving}>启用</CheckField><div><Btn variant="ghost" size="sm" isDisabled={state.saving} onPress={() => setEditing({ upstream: u, isNew: false })}><Pencil size={14} />编辑</Btn><Btn variant="ghost" size="sm" isIconOnly aria-label={`删除供应商 ${u.name}`} isDisabled={state.saving} onPress={() => setRemove(u)}><Trash2 size={15} /></Btn></div></div>
      <div className="upstream-card-test"><ConnectionTest url={u.base_url} /></div>
    </article>; })}</div> : <div className="panel"><Empty icon={<Network size={30} />} title="还没有配置供应商" action={<Btn onPress={add} isDisabled={state.saving}><Plus size={16} />添加第一个供应商</Btn>}>添加 newapi、sub2api 或官方服务的根地址并保存。无需在这里存放 API Key——调用模型使用供应商原本的 API Key；这里不生成业务密钥。</Empty></div>}
    <p className="page-note"><KeyRound size={14} />后台管理令牌只用于访问后台，与供应商 API Key 无关。专用地址忽略权重、匹配规则与附加请求头；停用供应商返回 503，未知标识符的 /标识符/v1/… 返回 404。旧入口（不带标识符）仍按原有路径 / 模型匹配与权重分配。</p>
    {editing && <UpstreamEditor key={editing.upstream.id + (editing.isNew ? ':new' : '')} initial={editing.upstream} isNew={editing.isNew} existing={draftUpstreams.filter(x => x.id !== editing.upstream.id).map(x => x.id)} onClose={() => setEditing(null)} onApply={apply} />}
    {remove && <Confirm title={`删除 ${remove.name}？`} label="从草稿中删除" danger onClose={() => setRemove(null)} onConfirm={() => { state.update(c => ({ ...c, upstreams: (c.upstreams ?? []).filter(u => u.id !== remove.id) })); setRemove(null); }}>该供应商将从配置草稿移除。保存配置后其专用地址 /{remove.id}/… 返回 404；历史日志不会被删除。</Confirm>}
  </div>;
}
