import { ArrowRight, Braces, Check, FlaskConical, Plus, Repeat2, ShieldAlert, SlidersHorizontal, Trash2, X } from 'lucide-react';
import { useRef, useState } from 'react';
import { api, errorMessage } from '../api';
import type { ConfigState } from '../hooks';
import { asNumber, inputNumber, number, validInteger } from '../lib';
import type { RetryConfig } from '../types';
import { Area, Btn, CheckField, Field, Notice, Panel } from '../components/ui';

const COMMON_CODES: [number, string][] = [[400, '请求异常'], [401, '未授权'], [403, '禁止访问'], [404, '未找到'], [408, '请求超时'], [409, '请求冲突'], [429, '请求限流'], [500, '服务异常'], [502, '网关错误'], [503, '服务不可用'], [504, '网关超时']];

function RegexTester({ patterns }: { patterns: string[] }) {
  const [pattern, setPattern] = useState(patterns[0] ?? '');
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<{ matched: boolean; error?: string } | null>(null);
  const generation = useRef(0);
  function editPattern(value: string) { generation.current++; setPattern(value); setResult(null); }
  function editText(value: string) { generation.current++; setText(value); setResult(null); }
  async function test() {
    const current = generation.current;
    setBusy(true); setResult(null);
    try {
      const value = await api<{ matched: boolean; error?: string }>('/regex/test', { method: 'POST', body: JSON.stringify({ pattern, text }) });
      if (current === generation.current) setResult(value);
    } catch (e) { if (current === generation.current) setResult({ matched: false, error: errorMessage(e) }); }
    finally { setBusy(false); }
  }
  return <Panel title={<><FlaskConical size={17} />规则测试台</>} caption="直接调用服务端 Go RE2 引擎，不发送上游请求。" className="regex-tester">
    <div className="panel-body form-stack">
      {patterns.length > 0 && <div className="field"><label htmlFor="pattern-template">从草稿规则载入</label><select id="pattern-template" value="" onChange={e => { if (e.target.value !== '') editPattern(patterns[Number(e.target.value)]); }}><option value="">选择一条表达式…</option>{patterns.map((p, i) => <option key={i} value={i}>{p || '（未填写）'}</option>)}</select></div>}
      <Field label="测试表达式" value={pattern} placeholder="(?i)rate limit" onChange={e => editPattern(e.target.value)} mono spellCheck={false} />
      <Area label="样本响应正文" value={text} placeholder="粘贴要检测的真实响应内容…" rows={7} onChange={e => editText(e.target.value)} spellCheck={false} />
      <Btn variant="secondary" busy={busy} isDisabled={!pattern.trim()} onPress={() => void test()}><FlaskConical size={15} />运行匹配测试</Btn>
      {result && <div className={`regex-result ${result.error ? 'regex-invalid' : result.matched ? 'regex-matched' : ''}`} role="status">{result.error ? <ShieldAlert size={18} /> : result.matched ? <Check size={18} /> : <X size={18} />}<div><strong>{result.error ? '测试失败 / 表达式无效' : result.matched ? '匹配成功' : '未匹配'}</strong><p>{result.error || (result.matched ? '此响应命中正文重试规则。' : '此响应未命中该表达式；状态码仍可能触发重试。')}</p></div></div>}
      <p className="field-hint">支持 Go RE2 语法，例如 (?i) 忽略大小写。不支持后向引用或环视；以服务端校验结果为准。</p>
    </div>
  </Panel>;
}

export function Retry({ state }: { state: ConfigState }) {
  const retry = state.draft!.retry;
  const codes = retry.status_codes ?? [];
  const patterns = retry.body_regexes ?? [];
  const [customCode, setCustomCode] = useState('');
  const [codeError, setCodeError] = useState('');
  const patch = (value: Partial<RetryConfig>) => state.update(c => ({ ...c, retry: { ...c.retry, ...value } }));
  function toggle(code: number, selected: boolean) { patch({ status_codes: selected ? [...new Set([...codes, code])].sort((a, b) => a - b) : codes.filter(c => c !== code) }); }
  function addCode() {
    const code = asNumber(customCode);
    if (!validInteger(code, 100) || code > 599) { setCodeError('请输入 100–599 的整数状态码。'); return; }
    if (codes.includes(code)) { setCodeError('此状态码已经选中。'); return; }
    toggle(code, true); setCustomCode(''); setCodeError('');
  }
  const additional = codes.filter(code => !COMMON_CODES.some(([common]) => common === code));
  const previewCount = Math.min(Math.max(0, retry.max_attempts - 1), 4);
  return <div className="page-stack">
    <div className="page-heading"><div><div className="eyebrow">RETRY POLICY</div><h1>重试规则</h1><p>用状态码或正文内容识别可恢复的错误，控制重试节奏。</p></div><span className="outline-label"><Repeat2 size={14} />在响应开始前执行</span></div>
    <div className="section-explainer"><Braces size={18} /><span><b>状态码命中</b> <span className="or-tag">或 OR</span> <b>任意完整正文正则命中</b> 即满足重试条件，仅在向客户端提交响应前执行。所有重试固定同一供应商，由总尝试次数限制；网络错误由独立开关控制。</span></div>
    <fieldset className="plain-fieldset" disabled={state.saving}>
      <div className="retry-layout"><div className="page-stack">
        <Panel title={<><span className="section-index">01</span>尝试次数与退避</>} caption="采用指数退避；可叠加随机抖动，避免集中重试。">
          <div className="panel-body form-stack"><div className="form-grid three"><Field label="总尝试次数" type="number" min={1} max={20} step={1} value={inputNumber(retry.max_attempts)} onChange={e => patch({ max_attempts: asNumber(e.target.value) })} hint="1–20 次，包含首次请求；1 表示不重试。" /><Field label="初始延迟（ms）" type="number" min={0} max={3_600_000} step={1} value={inputNumber(retry.base_delay_ms)} onChange={e => patch({ base_delay_ms: asNumber(e.target.value) })} /><Field label="延迟上限（ms）" type="number" min={0} max={3_600_000} step={1} value={inputNumber(retry.max_delay_ms)} onChange={e => patch({ max_delay_ms: asNumber(e.target.value) })} /></div>
            <div className="backoff-preview"><span className="backoff-node">首次请求</span>{Array.from({ length: Number.isFinite(previewCount) ? previewCount : 0 }, (_, i) => <span className="backoff-step" key={i}><span className="backoff-delay">{number(Math.min(retry.base_delay_ms * 2 ** i, retry.max_delay_ms))} ms<ArrowRight size={13} /></span><span className="backoff-node">重试 {i + 1}</span></span>)}{retry.max_attempts > 5 && <span>…</span>}</div>
            <p className="field-hint">{Number.isFinite(retry.max_attempts) ? `${retry.max_attempts} 次总尝试 = 最多 ${Math.max(0, retry.max_attempts - 1)} 次重试` : '请填写总尝试次数'}。以上为未叠加抖动的退避示意，不含请求耗时。</p>
            <div className="toggle-grid"><CheckField checked={retry.jitter} onChange={v => patch({ jitter: v })} disabled={state.saving} hint="让等待时间带有随机性，减少同时重试。">启用随机抖动</CheckField><CheckField checked={retry.retry_on_network_error} onChange={v => patch({ retry_on_network_error: v })} disabled={state.saving} hint="默认关闭；启用后可重试响应开始前的连接失败等网络错误。">重试网络错误</CheckField></div>
          </div>
        </Panel>
        <Panel title={<><span className="section-index">02</span>HTTP 状态码</>} caption="勾选常见状态码，也可以添加任意有效 HTTP 状态码。" action={<span className="soft-tag">{codes.length} 个已选</span>}>
          <div className="panel-body form-stack"><div className="codes-grid">{COMMON_CODES.map(([code, label]) => <CheckField key={code} checked={codes.includes(code)} onChange={v => toggle(code, v)} disabled={state.saving} className="code-checkbox"><strong className="mono">{code}</strong><span className="code-label">{label}</span></CheckField>)}</div>
            {additional.length > 0 && <div className="custom-codes">{additional.map(code => <span className="custom-code mono" key={code}>{code}<Btn variant="ghost" size="sm" isIconOnly aria-label={`移除状态码 ${code}`} isDisabled={state.saving} onPress={() => toggle(code, false)}><X size={13} /></Btn></span>)}</div>}
            <div className="custom-code-form"><Field label="其他 HTTP 状态码" type="number" min={100} max={599} step={1} placeholder="例如 520" value={customCode} onChange={e => { setCustomCode(e.target.value); setCodeError(''); }} error={codeError} onKeyDown={e => { if (e.key === 'Enter') { e.preventDefault(); addCode(); } }} /><Btn variant="secondary" onPress={addCode} isDisabled={state.saving}><Plus size={15} />添加</Btn></div>
            {codes.some(code => code >= 200 && code < 400) && <Notice>已选择成功或重定向状态码，这可能让正常响应再次发送到上游。请确认是预期行为。</Notice>}
          </div>
        </Panel>
        <Panel title={<><span className="section-index">03</span>响应正文规则</>} caption="每行一条独立正则。任意一条命中，就满足正文重试条件。" action={<Btn variant="ghost" size="sm" isDisabled={state.saving} onPress={() => patch({ body_regexes: [...patterns, ''] })}><Plus size={15} />添加规则</Btn>}>
          <div className="panel-body form-stack">{patterns.length ? patterns.map((pattern, index) => <div className="regex-row" key={index}><span className="rule-number mono">{String(index + 1).padStart(2, '0')}</span><Field label={`正文规则 ${index + 1}`} className="sr-label" value={pattern} placeholder="(?i)no available channel" mono spellCheck={false} onChange={e => patch({ body_regexes: patterns.map((p, i) => i === index ? e.target.value : p) })} /><Btn variant="ghost" isIconOnly aria-label={`删除正文规则 ${index + 1}`} isDisabled={state.saving} onPress={() => patch({ body_regexes: patterns.filter((_, i) => i !== index) })}><Trash2 size={15} /></Btn></div>) : <div className="quiet-empty"><Braces size={17} />没有正文规则。当前只按状态码和网络错误判断。</div>}<p className="field-hint">正则由服务端 Go RE2 编译。新增空规则需填写或删除后再保存。</p></div>
        </Panel>
      </div><div className="page-stack retry-aside"><RegexTester patterns={patterns} /><div className="safety-note"><ShieldAlert size={19} /><div><h3>注意重复生成与计费</h3><p>重试 POST 请求可能导致上游重复生成或重复计费。请谨慎选择错误条件与总尝试次数。</p><p>只在响应开始前重试。成功 SSE 立即透传，不逐帧匹配正则；压缩正文、嗅探超时或超过缓冲区上限的正文会跳过正则。已经开始发送的响应不会重放。</p></div></div><div className="aside-foot"><SlidersHorizontal size={16} />修改只影响保存后的新请求。</div></div></div>
    </fieldset>
  </div>;
}
