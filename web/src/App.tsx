import { Activity, ArrowRight, Check, ChevronRight, CircleHelp, FileClock, KeyRound, LayoutDashboard, LoaderCircle, LockKeyhole, Network, RefreshCw, Repeat2, Save, Settings2, Terminal, Undo2 } from 'lucide-react';
import { useCallback, useEffect, useState } from 'react';
import { useConfig, usePolling } from './hooks';
import type { LogEntry, LogsResponse, Page, Stats } from './types';
import { Btn, Confirm, Dialog, Empty, Notice } from './components/ui';
import { Overview } from './pages/Overview';
import { Upstreams } from './pages/Upstreams';
import { Retry } from './pages/Retry';
import { Logs } from './pages/Logs';
import { LogDetail } from './pages/LogDetail';
import { Settings, TokenForm } from './pages/Settings';

const NAV = [
  { id: 'overview', label: '运行总览', icon: LayoutDashboard, detail: 'Overview' },
  { id: 'upstreams', label: '供应商管理', icon: Network, detail: 'Providers' },
  { id: 'retry', label: '重试规则', icon: Repeat2, detail: 'Retry policy' },
  { id: 'logs', label: '请求日志', icon: FileClock, detail: 'Request logs' },
  { id: 'settings', label: '网关设置', icon: Settings2, detail: 'Settings' },
] as const;

export default function App() {
  const [page, setPage] = useState<Page>('overview');
  const [revision, setRevision] = useState(0);
  const [live, setLive] = useState(true);
  const [selectedLog, setSelectedLog] = useState<LogEntry | null>(null);
  const [authError, setAuthError] = useState<{ status: number; message: string } | null>(null);
  const [tokenOpen, setTokenOpen] = useState(false);
  const [discardOpen, setDiscardOpen] = useState(false);
  const [openNew, setOpenNew] = useState(false);
  const config = useConfig();
  const stats = usePolling<Stats>('/stats', 5_000, revision);
  const logs = usePolling<LogsResponse>('/logs?limit=50', live ? 3_000 : null, revision);
  const health = usePolling<{ ok: boolean }>('/health', 15_000, revision);
  const online = health.data?.ok === true && !health.error;
  const rows = logs.data?.logs ?? [];

  useEffect(() => {
    const handle = (event: Event) => setAuthError((event as CustomEvent<{ status: number; message: string }>).detail);
    const openToken = () => setTokenOpen(true);
    window.addEventListener('relay:auth-error', handle);
    window.addEventListener('relay:request-token', openToken);
    return () => { window.removeEventListener('relay:auth-error', handle); window.removeEventListener('relay:request-token', openToken); };
  }, []);
  useEffect(() => { document.title = `${NAV.find(item => item.id === page)!.label} · Agent Relay 重试网关`; }, [page]);
  const onNewHandled = useCallback(() => setOpenNew(false), []);
  function navigate(next: Page) { setPage(next); window.scrollTo({ top: 0, behavior: 'instant' }); }
  function addUpstream() { setOpenNew(true); navigate('upstreams'); }
  function tokenApplied() {
    setAuthError(null); setRevision(value => value + 1);
    // A token change never replaces an already editable config draft.
    if (!config.draft) void config.load();
  }
  async function save() { if (await config.save()) setRevision(value => value + 1); }
  const unavailable = !config.draft && (page === 'upstreams' || page === 'retry');

  return <div className="app-shell">
    <a className="skip-link" href="#main-content">跳到主要内容</a>
    <aside className="sidebar">
      <button className="brand" onClick={() => navigate('overview')} aria-label="Agent Relay 运行总览"><span className="brand-mark"><Repeat2 size={25} strokeWidth={2.3} /></span><span><strong>Agent Relay<span className="brand-dot">.</span></strong><small>重试网关</small></span></button>
      <div className="workspace-selector"><span className="workspace-icon"><Terminal size={15} /></span><div><strong>网关控制台</strong><span>SELF-HOSTED WORKSPACE</span></div><span className={`health-dot ${online ? '' : 'dot-muted'}`} /></div>
      <div className="nav-eyebrow">WORKSPACE</div>
      <nav aria-label="主要导航">{NAV.map(item => <button key={item.id} className={`nav-item ${page === item.id ? 'active' : ''}`} aria-current={page === item.id ? 'page' : undefined} onClick={() => navigate(item.id)}><item.icon size={18} /><span>{item.label}</span>{item.id === 'upstreams' && config.draft && <b className="nav-count">{config.draft.upstreams?.length ?? 0}</b>}{page === item.id && <span className="nav-active-dot" />}</button>)}</nav>
      <div className="sidebar-bottom"><div className="sidebar-note"><div><Activity size={15} /><strong>保持请求流动</strong></div><p>透明转发，有限重试。<br />让每一次异常都可追溯。</p><button onClick={() => navigate('retry')}>检查重试策略<ArrowRight size={13} /></button></div><button className="sidebar-security" onClick={() => navigate('settings')}><LockKeyhole size={15} /><span>访问与安全</span><ChevronRight size={14} /></button><div className="sidebar-version"><span className="mini-logo"><Repeat2 size={12} /></span>AGENT RELAY<span>GO + REACT</span></div></div>
    </aside>
    <div className="main-shell">
      <header className="topbar"><div className="breadcrumb"><span>控制台</span><ChevronRight size={13} /><strong>{NAV.find(item => item.id === page)?.label}</strong></div><div className="topbar-right"><span className={`connection-badge ${online ? 'is-online' : ''}`}><span className={`health-dot ${online ? '' : health.loading ? 'dot-pending' : 'dot-error'}`} />{online ? '进程在线' : health.loading && !health.data ? '连接中' : '连接异常'}</span><span className="topbar-divider" /><Btn variant="ghost" isIconOnly aria-label="管理接口令牌" onPress={() => setTokenOpen(true)}><KeyRound size={17} /></Btn></div></header>
      <div className={`config-bar ${config.dirty ? 'is-dirty' : ''}`}><div className="config-state">{config.saving ? <LoaderCircle size={15} className="spin" /> : config.dirty ? <span className="dirty-indicator" /> : <Check size={15} />}<span>{config.saving ? '正在保存配置…' : config.dirty ? '有未保存的配置修改' : config.savedAt ? '配置已保存' : config.draft ? '配置已同步' : '等待加载配置'}</span><span className="config-subtitle">{config.dirty ? '草稿仅在当前页面保留，保存后生效' : '代理配置 · 本地持久化'}</span></div><div className="inline-actions">{config.dirty && <Btn variant="ghost" size="sm" isDisabled={config.saving} onPress={() => setDiscardOpen(true)}><Undo2 size={14} />放弃修改</Btn>}<Btn size="sm" isDisabled={!config.dirty || config.loading || !config.draft} busy={config.saving} onPress={() => void save()}><Save size={14} />保存配置</Btn></div></div>
      <main id="main-content" tabIndex={-1}>
        <div className="global-notices">
          {authError && <Notice tone="error" action={<Btn variant="secondary" size="sm" onPress={() => setTokenOpen(true)}>{authError.status === 401 ? '填写令牌' : '访问设置'}</Btn>}><strong>HTTP {authError.status}</strong> · {authError.message}</Notice>}
          {config.error && <Notice tone="error" action={<Btn variant="secondary" size="sm" busy={config.loading} onPress={() => void config.load()}><RefreshCw size={14} />重试加载</Btn>}>无法加载配置：{config.error} 现有配置不会被默认值覆盖。</Notice>}
          {config.saveError && <Notice tone="error"><strong>配置未保存</strong><p className="pre-line">{config.saveError}</p><p>草稿已保留。修正后再保存。</p></Notice>}
        </div>
        {unavailable ? <section className="panel"><Empty icon={config.loading ? <LoaderCircle size={28} className="spin" /> : <CircleHelp size={28} />} title={config.loading ? '正在加载网关配置' : '配置尚不可用'} action={!config.loading ? <Btn variant="secondary" onPress={() => void config.load()}>重新加载配置</Btn> : undefined}>编辑需要先获取服务端现有配置，避免覆盖尚未读取的参数。</Empty></section> : <>
          {page === 'overview' && <Overview config={config.saved} stats={stats.data} statsError={stats.error} logs={rows} logsError={logs.error} onPage={navigate} onLog={setSelectedLog} onAdd={addUpstream} dirty={config.dirty} />}
          {page === 'upstreams' && <Upstreams state={config} openNew={openNew} onNewHandled={onNewHandled} />}
          {page === 'retry' && <Retry state={config} />}
          {page === 'logs' && <Logs logs={rows} total={logs.data?.total ?? 0} loading={logs.loading} error={logs.error} updated={logs.updated} live={live} onLive={setLive} onRefresh={logs.refresh} onSelect={setSelectedLog} onCleared={() => { setSelectedLog(null); logs.refresh(); stats.refresh(); }} />}
          {page === 'settings' && <Settings state={config} onTokenApplied={tokenApplied} />}
        </>}
        <footer className="main-footer"><span>AGENT RELAY <span className="muted">/</span> 重试网关</span><span>为可观测的 LLM 请求而构建</span></footer>
      </main>
    </div>
    {selectedLog && <LogDetail key={selectedLog.id} log={selectedLog} onClose={() => setSelectedLog(null)} />}
    {discardOpen && <Confirm title="放弃所有未保存的修改？" label="放弃修改" danger onClose={() => setDiscardOpen(false)} onConfirm={() => { config.discard(); setDiscardOpen(false); }}>上游、重试规则与网关设置中的草稿修改都会恢复到上次加载或保存的状态。服务端配置不会改变。</Confirm>}
    {tokenOpen && <Dialog title="管理接口访问" eyebrow="SESSION ACCESS" onClose={() => setTokenOpen(false)} footer={<Btn variant="secondary" onPress={() => setTokenOpen(false)}>完成</Btn>}><div className="form-stack">{authError && <Notice tone="error">{authError.message}</Notice>}<TokenForm onApplied={tokenApplied} /><p className="field-hint">令牌必须与服务端 AGENTROUTER_ADMIN_TOKEN 一致。未设置服务端令牌时，管理接口仅允许 loopback 本机访问；远程访问返回 403。</p></div></Dialog>}
  </div>;
}
