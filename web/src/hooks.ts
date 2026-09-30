import { useCallback, useEffect, useRef, useState } from 'react';
import { api, errorMessage } from './api';
import { configShape, validateConfig } from './lib';
import type { Config } from './types';

export function usePolling<T>(path: string, interval: number | null, revision: number) {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(true);
  const [updated, setUpdated] = useState<Date | null>(null);
  const [nonce, setNonce] = useState(0);
  useEffect(() => {
    let active = true;
    let running = false;
    const controller = new AbortController();
    async function load() {
      if (running || !active) return;
      running = true;
      setLoading(true);
      try {
        const result = await api<T>(path, { signal: controller.signal });
        if (active) { setData(result); setError(''); setUpdated(new Date()); }
      } catch (e) {
        if (active && !controller.signal.aborted) setError(errorMessage(e));
      } finally {
        running = false;
        if (active) setLoading(false);
      }
    }
    void load();
    const timer = interval ? window.setInterval(() => { if (!document.hidden) void load(); }, interval) : undefined;
    return () => { active = false; controller.abort(); window.clearInterval(timer); };
  }, [path, interval, revision, nonce]);
  return { data, error, loading, updated, refresh: useCallback(() => setNonce(n => n + 1), []) };
}

export function useConfig() {
  const [saved, setSaved] = useState<Config | null>(null);
  const [draft, setDraft] = useState<Config | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  const [saveError, setSaveError] = useState('');
  const [savedAt, setSavedAt] = useState<Date | null>(null);
  const locked = useRef(false);
  const loadSequence = useRef(0);
  const dirty = JSON.stringify(draft) !== JSON.stringify(saved);

  const load = useCallback(async () => {
    const sequence = ++loadSequence.current;
    setLoading(true);
    setError('');
    try {
      const value = await api<unknown>('/config');
      if (!configShape(value)) throw new Error('配置接口返回了无法识别的数据。为避免覆盖配置，编辑已暂停。');
      if (sequence !== loadSequence.current) return;
      setSaved(structuredClone(value));
      setDraft(structuredClone(value));
    } catch (e) { if (sequence === loadSequence.current) setError(errorMessage(e)); }
    finally { if (sequence === loadSequence.current) setLoading(false); }
  }, []);

  useEffect(() => { void load(); return () => { loadSequence.current++; }; }, [load]);
  useEffect(() => {
    if (!dirty) return;
    const prevent = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ''; };
    window.addEventListener('beforeunload', prevent);
    return () => window.removeEventListener('beforeunload', prevent);
  }, [dirty]);

  const update = useCallback((edit: (config: Config) => Config) => {
    if (locked.current) return;
    setDraft(current => current ? edit(current) : current);
    setSaveError('');
    setSavedAt(null);
  }, []);

  async function save(): Promise<boolean> {
    if (!draft || locked.current) return false;
    const errors = validateConfig(draft);
    if (errors.length) { setSaveError(errors.join('\n')); return false; }
    locked.current = true;
    setSaving(true);
    setSaveError('');
    // Full object, including unknown fields at every level. Never rebuild defaults.
    const snapshot = structuredClone(draft);
    try {
      await api('/config', { method: 'PUT', body: JSON.stringify(snapshot) });
      setSaved(snapshot);
      setSavedAt(new Date());
      return true;
    } catch (e) { setSaveError(errorMessage(e)); return false; }
    finally { locked.current = false; setSaving(false); }
  }

  function discard() {
    if (locked.current) return;
    setDraft(saved ? structuredClone(saved) : null);
    setSaveError('');
    setSavedAt(null);
  }

  return { saved, draft, update, dirty, loading, saving, error, saveError, savedAt, load, save, discard };
}

export type ConfigState = ReturnType<typeof useConfig>;
