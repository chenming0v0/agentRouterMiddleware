import { AlertTriangle, ArrowDownToLine, FileText, LoaderCircle } from 'lucide-react';
import { useEffect, useRef, useState } from 'react';
import { apiBlob, ApiError, errorMessage } from '../api';
import { byteSize, captureMessage, captureState, decodeBody, type CaptureInfo } from '../body';
import { downloadBlob } from '../lib';
import { Btn, CopyButton, Notice } from './ui';

export function LogBody({ logID, part, title, preview, shortened, capture, headers }: {
  logID: string; part: 'request' | 'response' | `attempt-${number}`; title: string;
  preview: string; shortened: boolean; capture: CaptureInfo; headers?: Record<string, string[]> | null;
}) {
  const [blob, setBlob] = useState<Blob | null>(null);
  const [text, setText] = useState<string | null | undefined>(undefined);
  const [showLoaded, setShowLoaded] = useState(false);
  const [busy, setBusy] = useState<'view' | 'download' | null>(null);
  const [error, setError] = useState('');
  const [authError, setAuthError] = useState(false);
  const pending = useRef<AbortController | null>(null);
  useEffect(() => () => { pending.current?.abort(); pending.current = null; }, [logID, part]);
  const state = captureState(capture);
  const partial = state === 'partial';
  const unavailable = capture.available === false;
  const readLabel = blob ? '查看已读取正文' : state === 'complete' ? '读取完整正文' : '读取已捕获正文';

  async function run(action: 'view' | 'download') {
    if (pending.current) return;
    const controller = new AbortController();
    pending.current = controller;
    setBusy(action); setError(''); setAuthError(false);
    try {
      const value = blob ?? await apiBlob(`/logs/${encodeURIComponent(logID)}/body/${part}`, controller.signal);
      if (controller.signal.aborted) return;
      setBlob(value);
      if (action === 'download') {
        downloadBlob(value, `request-${logID.replace(/[^a-z0-9_-]/gi, '_')}-${part}.bin`);
      } else {
        const decoded = text === undefined ? await decodeBody(value, headers) : text;
        if (controller.signal.aborted) return;
        setText(decoded); setShowLoaded(true);
      }
    } catch (e) {
      if (!controller.signal.aborted) {
        setError(errorMessage(e));
        setAuthError(e instanceof ApiError && (e.status === 401 || e.status === 403));
      }
    } finally {
      if (pending.current === controller) { pending.current = null; setBusy(null); }
    }
  }
  function cancel() { pending.current?.abort(); pending.current = null; setBusy(null); }
  const displayedText = showLoaded ? text : preview;
  return <section className="code-block body-capture" data-body-part={part} data-capture-state={state}>
    <div className="code-heading"><h3>{title}<span className="body-view-label">{showLoaded ? '已读取存档' : '列表预览'}</span></h3>
      {typeof displayedText === 'string' && <CopyButton text={displayedText} label={showLoaded ? `复制${title}（已读取）` : `复制${title}预览`} compact />}
    </div>
    <div className="body-capture-info"><span>已观测 {byteSize(capture.observed)}</span><span>已存储 {byteSize(capture.captured)}</span>{blob && <span>已读取 {byteSize(blob.size)}</span>}</div>
    <div className={`body-capture-message ${partial || unavailable ? 'capture-warning' : ''}`}>
      {partial || unavailable ? <AlertTriangle size={14} /> : <FileText size={14} />}<span>{showLoaded && state === 'complete' && !unavailable ? '完整存档已读取。下方展示实际获取的正文，不再使用列表预览。' : captureMessage(capture, shortened)}</span>
    </div>
    {showLoaded && text === null ? <div className="body-binary"><FileText size={23} /><p>这是压缩、二进制或无法可靠解码的正文，不转换为文本显示。</p><p>请下载原始文件；下载会保留全部已捕获字节，不做 UTF-8 重编码。</p></div> : <pre tabIndex={0} aria-label={`${title}${showLoaded ? '（已读取）' : '预览'}`}>{displayedText || <span className="muted">{showLoaded ? '空正文（0 字节）' : '无预览内容'}</span>}</pre>}
    {error && <div className="body-action-error"><Notice tone="error" action={authError ? <Btn variant="secondary" size="sm" onPress={() => window.dispatchEvent(new Event('relay:request-token'))}>访问设置</Btn> : undefined}>{error}</Notice></div>}
    <div className="body-actions">
      <Btn variant="secondary" size="sm" isDisabled={unavailable || busy !== null} onPress={() => void run('view')} aria-label={`${title}：${readLabel}`}>
        {busy === 'view' ? <LoaderCircle size={14} className="spin" /> : <FileText size={14} />}{busy === 'view' ? '正在读取…' : readLabel}
      </Btn>
      <Btn variant="ghost" size="sm" isDisabled={unavailable || busy !== null} onPress={() => void run('download')} aria-label={`下载${title}原始文件`}>
        {busy === 'download' ? <LoaderCircle size={14} className="spin" /> : <ArrowDownToLine size={14} />}{busy === 'download' ? '正在下载…' : '下载原始文件'}
      </Btn>
      {busy && <Btn variant="ghost" size="sm" onPress={cancel}>取消读取</Btn>}
      {showLoaded && !busy && <Btn variant="ghost" size="sm" onPress={() => setShowLoaded(false)}>查看预览</Btn>}
    </div>
    {(capture.captured ?? 0) > 8 * 1024 * 1024 && <p className="body-memory-note">正文较大，文本显示会占用浏览器内存，建议优先下载原始文件。</p>}
  </section>;
}
