import { Activity, ArrowDownRight, ArrowRight, ArrowUpRight, CheckCheck, CircleHelp, Clock3, FileClock, Globe2, Network, Plus, Repeat2, Server, ShieldCheck, Unplug, Zap } from 'lucide-react';
import type { Config, LogEntry, Page, Stats } from '../types';
import { duration, number, providerBaseURL, time, uptime, validUpstreamID } from '../lib';
import { Btn, Empty, Notice, Panel, Status } from '../components/ui';

export function Overview({ config, stats, statsError, logs, logsError, onPage, onLog, onAdd, dirty }: {
  config: Config | null; stats: Stats | null; statsError: string; logs: LogEntry[]; logsError: string;
  onPage: (page: Page) => void; onLog: (log: LogEntry) => void; onAdd: () => void; dirty: boolean;
}) {
  const upstreams = config?.upstreams ?? [];
  const routable = upstreams.filter(u => u.enabled && validUpstreamID(u.id));
  const cards = [
    { label: '代理请求', value: stats?.total_requests, icon: Activity, caption: '网关累计接收', tone: '' },
    { label: '触发重试', value: stats?.retried_requests, icon: Repeat2, caption: '所有尝试固定同一供应商', tone: 'amber' },
    { label: '重试成功', value: stats?.retry_success, icon: CheckCheck, caption: '重试后恢复的请求', tone: 'lime' },
    { label: '最终失败', value: stats?.failed_requests, icon: Unplug, caption: '含错误、取消或未完整结束', tone: 'rose' },
  ];
  return <div className="page-stack">
    <div className="page-heading"><div><div className="eyebrow">GATEWAY OVERVIEW</div><h1>网关运行总览</h1><p>查看请求统计、供应商入口与最近的响应。</p></div>
      <div className="heading-detail"><Clock3 size={15} /><span>已运行<br /><strong>{stats ? uptime(stats.uptime_seconds) : '—'}</strong></span></div>
    </div>
    {statsError && <Notice tone="error">统计获取失败：{statsError}{stats && ' 当前显示的是上次成功获取的数据。'}</Notice>}
    <div className="stats-grid">{cards.map(({ label, value, icon: Icon, caption, tone }) => <div className={`stat-card ${tone}`} key={label}>
      <div className="stat-label">{label}<Icon size={17} /></div><div className="stat-value">{value === undefined ? '—' : number(value)}</div><div className="stat-caption"><span className="tiny-dash" />{caption}</div>
    </div>)}</div>
    {config && !routable.length && <div className="setup-banner"><div className="setup-icon"><Network size={27} /></div><div><h2>接入第一个供应商</h2><p>还没有可接收流量的供应商。添加根地址并保存，即可复制专用地址并用该供应商原本的 API Key 调用。</p></div><Btn onPress={onAdd}><Plus size={16} />添加供应商</Btn></div>}
    <div className="overview-grid">
      <Panel title={<><Network size={17} />供应商入口</>} caption="调用方请求专用地址 /标识符/v1/…，使用供应商原本的 API Key。" action={<Btn variant="ghost" size="sm" onPress={() => onPage('upstreams')}>管理<ArrowUpRight size={15} /></Btn>}>
        <div className="route-flow"><div className="flow-node"><span className="flow-icon"><ArrowDownRight size={20} /></span><span>调用方<small>+/ID/V1/…</small></span></div><span className="flow-line" /><div className="flow-node relay-node"><span className="flow-icon"><Repeat2 size={20} /></span><span>重试网关<small>AGENT RELAY</small></span></div><span className="flow-line" /><div className="flow-node"><span className="flow-icon"><Server size={20} /></span><span>{routable.length} 个供应商<small>ENABLED</small></span></div></div>
        <div className="upstream-summary">{routable.length ? routable.slice(0, 4).map(u => <div className="upstream-summary-row" key={u.id}><span className="health-dot" /><div className="summary-name"><strong>{u.name}</strong><span className="mono">{providerBaseURL(u.id)}/v1/…</span></div><div className="summary-weight"><Globe2 size={13} /><span className="mono truncate">{u.base_url}</span></div></div>) : <div className="quiet-empty"><CircleHelp size={16} />保存并启用供应商后，专用入口将在此显示。</div>}</div>
        <div className="panel-foot"><ShieldCheck size={14} /><span>{routable.length ? `${routable.length} 个有可用的专用地址 · ${upstreams.length - routable.length} 个已停用或标识符无效` : '暂未接入有效供应商'}{dirty ? ' · 展示已保存配置' : ''}</span></div>
      </Panel>
      <Panel title={<><Zap size={17} />当前重试策略</>} action={<Btn variant="ghost" size="sm" onPress={() => onPage('retry')}>调整<ArrowUpRight size={15} /></Btn>}>
        <div className="policy-highlight"><span className="big-number">{config?.retry.max_attempts ?? '—'}</span><div><strong>次总尝试</strong><p>含首次请求 · 最多 {config ? Math.max(0, config.retry.max_attempts - 1) : '—'} 次重试</p></div><Repeat2 size={30} /></div>
        <div className="policy-values"><div><span>初始退避</span><strong className="mono">{config ? `${number(config.retry.base_delay_ms)} ms` : '—'}</strong></div><div><span>退避上限</span><strong className="mono">{config ? `${number(config.retry.max_delay_ms)} ms` : '—'}</strong></div></div>
        <div className="policy-tags"><span className="soft-tag">{config ? (config.retry.status_codes ?? []).length : '—'} 个状态码</span><span className="soft-tag">{config ? (config.retry.body_regexes ?? []).length : '—'} 条正文规则</span><span className="soft-tag">抖动 {config ? config.retry.jitter ? '开启' : '关闭' : '—'}</span></div>
        <div className="panel-foot"><CircleHelp size={14} /><span>仅在向客户端开始响应之前重试。</span></div>
      </Panel>
    </div>
    <Panel title={<><FileClock size={17} />最近请求</>} caption="最新记录在前，时间列显示请求开始时间。" action={<Btn variant="ghost" size="sm" onPress={() => onPage('logs')}>全部日志<ArrowRight size={15} /></Btn>}>
      {logsError && <div className="inset-notice"><Notice tone="error">日志获取失败：{logsError}</Notice></div>}
      {logs.length ? <div className="table-scroll"><table className="request-table"><thead><tr><th>时间 / 路径</th><th>模型</th><th>上游</th><th>状态</th><th>尝试</th><th>耗时</th><th><span className="sr-only">详情</span></th></tr></thead><tbody>{logs.slice(0, 5).map(log => <tr key={log.id}><td><button className="table-link" onClick={() => onLog(log)}><span className="mono muted small">{time(log.time)} <b className="method">{log.method}</b></span><strong className="mono truncate">{log.path}</strong></button></td><td className="mono">{log.model || '—'}</td><td>{log.upstream_name || '—'}</td><td><Status code={log.status} /></td><td><span className={`attempt-chip ${log.attempt_count > 1 ? 'has-retry' : ''}`}>{log.attempt_count} 次</span></td><td className="mono nowrap">{duration(log.duration_ms)}</td><td><Btn variant="ghost" isIconOnly size="sm" aria-label={`查看请求 ${log.id}`} onPress={() => onLog(log)}><ArrowUpRight size={16} /></Btn></td></tr>)}</tbody></table></div> : <Empty icon={<FileClock size={28} />} title={logsError ? '日志暂不可用' : '等待第一条请求'}>网关不会生成模拟流量。客户端请求经过代理后，记录会出现在这里。</Empty>}
      <div className="panel-foot spread"><span>最多显示最近 5 条</span><span className="mono">STATS / 5s &nbsp; LOGS / 3s</span></div>
    </Panel>
  </div>;
}
