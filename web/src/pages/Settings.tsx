import { Database, HardDrive, KeyRound, LockKeyhole, Network, Save, Settings2, ShieldCheck } from 'lucide-react';
import { useState } from 'react';
import { errorMessage, getToken, storeToken } from '../api';
import type { ConfigState } from '../hooks';
import { asNumber, bytes, inputNumber, MAX_BYTES } from '../lib';
import { Btn, Field, Notice, Panel } from '../components/ui';

export function TokenForm({ onApplied }: { onApplied: () => void }) {
  const [token, setToken] = useState(getToken);
  const [visible, setVisible] = useState(false);
  const [feedback, setFeedback] = useState('');
  const [error, setError] = useState('');
  function apply() {
    try {
      storeToken(token);
      setFeedback(token.trim() ? '令牌已存入当前会话。正在重新连接管理接口。' : '已清除会话令牌。正在使用本机访问模式重新连接。');
      setError(''); onApplied();
    } catch (e) { setError(`会话存储不可用：${errorMessage(e)}`); }
  }
  return <form className="form-stack" onSubmit={e => { e.preventDefault(); apply(); }}>
    <div className="token-input"><Field label="管理员令牌" type={visible ? 'text' : 'password'} placeholder="输入 AGENTROUTER_ADMIN_TOKEN" value={token} onChange={e => { setToken(e.target.value); setFeedback(''); }} autoComplete="off" spellCheck={false} mono /><Btn variant="ghost" onPress={() => setVisible(!visible)} aria-label={visible ? '隐藏管理员令牌' : '显示管理员令牌'}>{visible ? '隐藏' : '显示'}</Btn></div>
    <p className="field-hint">仅存于当前标签页的 sessionStorage，关闭会话即清除。不写入配置文件、URL 或 localStorage。每次管理 API 请求通过 Authorization: Bearer 携带。后台管理令牌只用于访问后台，与供应商 API Key 无关。</p>
    <div className="inline-actions"><Btn variant="secondary" type="submit"><KeyRound size={15} />应用会话令牌</Btn>{getToken() && <Btn variant="ghost" onPress={() => { try { storeToken(''); setToken(''); setFeedback('令牌已清除。'); setError(''); onApplied(); } catch (e) { setError(errorMessage(e)); } }}>清除令牌</Btn>}</div>
    {feedback && <Notice tone="success">{feedback}</Notice>}{error && <Notice tone="error">{error}</Notice>}
  </form>;
}

export function Settings({ state, onTokenApplied }: { state: ConfigState; onTokenApplied: () => void }) {
  const config = state.draft;
  const patch = (key: string, value: string | number) => state.update(c => ({ ...c, [key]: value }));
  return <div className="page-stack settings-page">
    <div className="page-heading"><div><div className="eyebrow">GATEWAY SETTINGS</div><h1>网关设置</h1><p>管理运行参数、日志保留和管理界面的访问凭据。</p></div><span className="outline-label"><Settings2 size={14} />本地配置</span></div>
    <Panel title={<><LockKeyhole size={18} />管理接口访问</>} caption="管理令牌与转发到上游的模型 API Key 完全独立。"><div className="panel-body form-stack"><TokenForm onApplied={onTokenApplied} /><div className="security-info"><ShieldCheck size={18} /><p>服务端未设置 <code>AGENTROUTER_ADMIN_TOKEN</code> 时，仅允许 localhost / loopback 直接访问管理接口。远程访问返回 403；配置令牌后，访问方必须提交正确令牌。健康检查不需要令牌。</p></div></div></Panel>
    {config ? <fieldset className="plain-fieldset page-stack" disabled={state.saving}>
      <Panel title={<><Network size={18} />服务监听</>} caption="更改监听地址后，需要手动重启 Go 服务。"><div className="panel-body"><div className="form-grid"><Field label="监听地址" value={config.listen} placeholder="127.0.0.1:18851" onChange={e => patch('listen', e.target.value)} hint="默认 127.0.0.1:18851。绑定所有接口（如 :18851）必须配置管理员令牌；此项不会热更新。" mono /><div className="setting-callout"><Save size={18} /><p>保存配置不等于重启服务。重启后，请使用新地址重新打开控制台。</p></div></div></div></Panel>
      <Panel title={<><Database size={18} />日志保留</>} caption="默认保留最近 50 条记录及正文存档，用于诊断而非长期审计。"><div className="panel-body form-stack"><div className="form-grid"><Field label="保留条数" value={inputNumber(config.log_limit)} type="number" min={1} max={1000} step={1} onChange={e => patch('log_limit', asNumber(e.target.value))} hint="1–1000 条，默认 50 条。减小条数会淘汰较旧记录及其正文文件。" /><Field label="单份日志正文上限（bytes）" value={inputNumber(config.max_log_body_bytes)} type="number" min={0} max={MAX_BYTES} step={1} onChange={e => patch('max_log_body_bytes', asNumber(e.target.value))} hint={config.max_log_body_bytes === 0 ? '0 = 不限制磁盘捕获长度；完整正文流式落盘，不是全部保存在 RAM。列表仅返回最多 32 KiB 预览。' : `当前上限 ${bytes(config.max_log_body_bytes)}，最大 1 GiB。超过该显式上限时，存档会标记实际捕获不完整。`} /></div>{config.max_log_body_bytes === 0 && <Notice>完整正文可能包含大量流式输出、提示词与凭据。请关注磁盘空间和日志文件访问权限；客户端取消或 I/O 错误仍可能造成捕获不完整。</Notice>}</div></Panel>
      <details className="panel advanced-settings"><summary><span><HardDrive size={18} /><strong>高级资源限制</strong></span><span className="muted small">请求体、缓冲区与读取时限</span></summary><div className="panel-body">
        <div className="form-grid"><Field label="最大请求体（bytes）" value={inputNumber(config.max_request_body_bytes)} type="number" min={1} max={MAX_BYTES} step={1} onChange={e => patch('max_request_body_bytes', asNumber(e.target.value))} hint={`当前 ${bytes(config.max_request_body_bytes)}，最大 1 GiB。限制单个客户端请求体大小。`} /><Field label="最大响应缓冲区（bytes）" value={inputNumber(config.max_buffer_bytes)} type="number" min={1} max={MAX_BYTES} step={1} onChange={e => patch('max_buffer_bytes', asNumber(e.target.value))} hint={`当前 ${bytes(config.max_buffer_bytes)}，最大 1 GiB。正文超过此上限时跳过正则检测。`} /></div>
        <div className="form-grid advanced-hint"><Field label="正文嗅探时限（ms）" type="number" min={1} max={60_000} step={1} value={inputNumber(config.body_sniff_timeout_ms ?? 250)} onChange={e => patch('body_sniff_timeout_ms', asNumber(e.target.value))} hint="1–60000 ms，默认 250 ms。读取太慢会跳过正则，避免阻塞正常响应。" /><Field label="错误正文读取时限（ms）" type="number" min={1} max={60_000} step={1} value={inputNumber(config.error_body_timeout_ms ?? 2000)} onChange={e => patch('error_body_timeout_ms', asNumber(e.target.value))} hint="1–60000 ms，默认 2000 ms。限制错误响应在重试前的正文读取等待。" /></div>
        <p className="field-hint advanced-hint">预览省略与实际捕获不完整是两回事。请在请求详情核对捕获状态、已观测 / 已存储字节数及错误；完整磁盘存档不会自动读入管理页面。</p>
      </div></details>
    </fieldset> : <Notice tone="info">配置尚未加载。先完成管理接口连接，再编辑监听与日志参数；不会用默认值覆盖现有配置。</Notice>}
  </div>;
}
