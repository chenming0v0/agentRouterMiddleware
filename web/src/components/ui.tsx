import { Button, Checkbox, Description, Input, TextArea } from '@heroui/react';
import { AlertTriangle, Check, Copy, LoaderCircle, X } from 'lucide-react';
import { useEffect, useId, useRef, useState, type ComponentProps, type ReactNode } from 'react';

type ButtonProps = Omit<ComponentProps<typeof Button>, 'className' | 'children'> & {
  children?: ReactNode; className?: string; busy?: boolean;
};
export function Btn({ children, busy, className = '', isDisabled, ...props }: ButtonProps) {
  return <Button {...props} type={props.type ?? 'button'} className={`relay-button ${className}`} isDisabled={isDisabled || busy} aria-busy={busy || undefined}>
    {busy && <LoaderCircle size={16} className="spin" aria-hidden="true" />}{children}
  </Button>;
}

type FieldProps = Omit<ComponentProps<typeof Input>, 'className'> & {
  label: ReactNode; hint?: ReactNode; error?: string; mono?: boolean; className?: string;
};
export function Field({ label, hint, error, mono, className = '', ...props }: FieldProps) {
  const uid = useId();
  const id = props.id ?? uid;
  return <div className={`field ${className}`}>
    <label htmlFor={id}>{label}</label>
    <Input {...props} id={id} className={`field-input ${mono ? 'mono' : ''}`} aria-invalid={!!error || undefined} aria-describedby={hint || error ? `${id}-help` : undefined} />
    {(hint || error) && <p id={`${id}-help`} className={error ? 'field-error' : 'field-hint'}>{error || hint}</p>}
  </div>;
}

type AreaProps = Omit<ComponentProps<typeof TextArea>, 'className'> & {
  label: ReactNode; hint?: ReactNode; error?: string; mono?: boolean; className?: string;
};
export function Area({ label, hint, error, mono = true, className = '', ...props }: AreaProps) {
  const uid = useId();
  const id = props.id ?? uid;
  return <div className={`field ${className}`}>
    <label htmlFor={id}>{label}</label>
    <TextArea {...props} id={id} className={`field-input field-area ${mono ? 'mono' : ''}`} aria-invalid={!!error || undefined} aria-describedby={hint || error ? `${id}-help` : undefined} />
    {(hint || error) && <p id={`${id}-help`} className={error ? 'field-error' : 'field-hint'}>{error || hint}</p>}
  </div>;
}

export function CheckField({ checked, onChange, children, hint, disabled = false, className = '' }: {
  checked: boolean; onChange: (value: boolean) => void; children: ReactNode; hint?: string; disabled?: boolean; className?: string;
}) {
  return <Checkbox isSelected={checked} onChange={onChange} isDisabled={disabled} variant="primary" className={`relay-checkbox ${className}`}>
    <Checkbox.Content>
      <Checkbox.Control><Checkbox.Indicator /></Checkbox.Control>
      <span>{children}</span>
    </Checkbox.Content>
    {hint && <Description>{hint}</Description>}
  </Checkbox>;
}

export function Panel({ title, caption, action, children, className = '' }: {
  title?: ReactNode; caption?: ReactNode; action?: ReactNode; children: ReactNode; className?: string;
}) {
  return <section className={`panel ${className}`}>
    {(title || action) && <div className="panel-header"><div><h2>{title}</h2>{caption && <p>{caption}</p>}</div>{action}</div>}
    {children}
  </section>;
}

export function Notice({ children, tone = 'warning', action }: { children: ReactNode; tone?: 'warning' | 'error' | 'info' | 'success'; action?: ReactNode }) {
  return <div className={`notice notice-${tone}`} role={tone === 'error' ? 'alert' : 'status'}>
    {tone === 'success' ? <Check size={17} aria-hidden="true" /> : <AlertTriangle size={17} aria-hidden="true" />}
    <div>{children}</div>{action}
  </div>;
}

export function Empty({ icon, title, children, action }: { icon: ReactNode; title: string; children: ReactNode; action?: ReactNode }) {
  return <div className="empty-state"><div className="empty-icon">{icon}</div><h3>{title}</h3><p>{children}</p>{action}</div>;
}

export function Status({ code }: { code: number }) {
  const tone = code === 0 || code >= 500 ? 'error' : code >= 400 ? 'warning' : code >= 300 ? 'info' : code >= 200 ? 'success' : 'neutral';
  return <span className={`status status-${tone}`}><span />{code === 0 ? '网络错误' : code}</span>;
}

export function Dialog({ title, eyebrow, children, footer, onClose, wide = false, drawer = false }: {
  title: string; eyebrow?: string; children: ReactNode; footer?: ReactNode; onClose: () => void; wide?: boolean; drawer?: boolean;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  useEffect(() => {
    const dialog = ref.current;
    if (!dialog) return;
    dialog.showModal();
    return () => { dialog.close(); };
  }, []);
  return <dialog ref={ref} aria-labelledby={titleId} className={`relay-dialog ${wide ? 'dialog-wide' : ''} ${drawer ? 'dialog-drawer' : ''}`}
    onCancel={event => { event.preventDefault(); onClose(); }}
    onClick={event => {
      if (event.target !== event.currentTarget) return;
      const rect = event.currentTarget.getBoundingClientRect();
      if (event.clientX < rect.left || event.clientX > rect.right || event.clientY < rect.top || event.clientY > rect.bottom) onClose();
    }}>
    <div className="dialog-header"><div>{eyebrow && <span className="eyebrow">{eyebrow}</span>}<h2 id={titleId}>{title}</h2></div>
      <Btn variant="ghost" isIconOnly aria-label="关闭对话框" onPress={onClose}><X size={20} /></Btn>
    </div>
    <div className="dialog-body">{children}</div>
    {footer && <div className="dialog-footer">{footer}</div>}
  </dialog>;
}

export function Confirm({ title, children, label = '确认', onClose, onConfirm, busy = false, danger = false }: {
  title: string; children: ReactNode; label?: string; onClose: () => void; onConfirm: () => void; busy?: boolean; danger?: boolean;
}) {
  return <Dialog title={title} eyebrow="请确认此操作" onClose={() => { if (!busy) onClose(); }} footer={<>
    <Btn variant="secondary" isDisabled={busy} onPress={onClose}>取消</Btn>
    <Btn variant={danger ? 'danger' : 'primary'} busy={busy} onPress={onConfirm}>{label}</Btn>
  </>}><div className="confirmation-copy">{children}</div></Dialog>;
}

export function CopyButton({ text, label = '复制', compact = false }: { text: string; label?: string; compact?: boolean }) {
  const [state, setState] = useState<'idle' | 'copied' | 'error'>('idle');
  useEffect(() => {
    if (state === 'idle') return;
    const timer = setTimeout(() => setState('idle'), 3_000);
    return () => clearTimeout(timer);
  }, [state]);
  async function copy() {
    try {
      if (navigator.clipboard?.writeText) await navigator.clipboard.writeText(text);
      else {
        const area = document.createElement('textarea');
        area.value = text;
        area.style.position = 'fixed';
        area.style.opacity = '0';
        // Append inside an active modal so the native dialog focus trap permits selection.
        (document.querySelector('dialog[open]') ?? document.body).appendChild(area);
        area.select();
        const success = document.execCommand('copy');
        area.remove();
        if (!success) throw new Error('copy failed');
      }
      setState('copied');
    } catch { setState('error'); }
  }
  return <span className="copy-control"><Btn variant="ghost" size="sm" isIconOnly={compact} aria-label={label} onPress={() => void copy()}>
    {state === 'copied' ? <Check size={14} /> : <Copy size={14} />}{!compact && (state === 'copied' ? '已复制' : label)}
  </Btn>{state === 'error' && <small role="alert">复制失败，请手动选择内容。</small>}</span>;
}

export function CodeBlock({ title, value, truncated = false }: { title: string; value: string; truncated?: boolean }) {
  return <section className="code-block">
    <div className="code-heading"><h3>{title}</h3><CopyButton text={value} label={`复制${title}`} compact /></div>
    {truncated && <div className="truncated-warning"><AlertTriangle size={14} />正文已被截断，以下不是完整内容。</div>}
    <pre tabIndex={0} aria-label={title}>{value || <span className="muted">无内容</span>}</pre>
  </section>;
}
