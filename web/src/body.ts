export interface CaptureInfo {
  observed?: number;
  captured?: number;
  complete?: boolean;
  available?: boolean;
  error?: string;
}

export function captureInfo(source: Record<string, unknown>, prefix: 'request' | 'response'): CaptureInfo {
  return {
    observed: source[`${prefix}_body_bytes`] as number | undefined,
    captured: source[`${prefix}_body_captured_bytes`] as number | undefined,
    complete: source[`${prefix}_body_complete`] as boolean | undefined,
    available: source[`${prefix}_body_available`] as boolean | undefined,
    error: source[`${prefix}_body_error`] as string | undefined,
  };
}

export function captureState(info: CaptureInfo): 'complete' | 'partial' | 'unknown' {
  if (info.complete === false || info.error) return 'partial';
  return info.complete === true ? 'complete' : 'unknown';
}

export function captureMessage(info: CaptureInfo, shortened: boolean): string {
  if (info.available === false) return `存档正文不可用，仅能查看已有预览。${info.error ? `原因：${info.error}` : ''}`;
  if (captureState(info) === 'partial') return `实际捕获不完整：${info.error || '达到捕获上限、请求中断或读取未完成'}。读取和下载仅包含已捕获部分。`;
  if (info.complete === true) return shortened ? '预览已省略，完整正文可读取。' : '完整正文已捕获，可按需读取或下载。';
  return shortened ? '预览已省略；当前元数据未提供完整捕获状态，不能据此判断存档是否完整。' : '当前元数据未提供完整捕获状态。';
}

export function byteSize(value: number | undefined) {
  if (value === undefined) return '未知';
  const formatted = new Intl.NumberFormat('zh-CN').format(value);
  const unit = value >= 1_048_576 ? `${+(value / 1_048_576).toFixed(2)} MiB` : value >= 1024 ? `${+(value / 1024).toFixed(2)} KiB` : '';
  return unit ? `${unit} · ${formatted} B` : `${formatted} B`;
}

// A viewing aid, not a conversion step. Downloads ALWAYS use the original Blob.
export async function decodeBody(blob: Blob, headers: Record<string, string[]> | null | undefined): Promise<string | null> {
  const header = (name: string) => Object.entries(headers ?? {}).find(([key]) => key.toLowerCase() === name)?.[1]?.join(';') ?? '';
  const encoding = header('content-encoding').toLowerCase().trim();
  const contentType = header('content-type').toLowerCase();
  if (encoding && encoding !== 'identity') return null;
  if (/^(image|audio|video)\/|application\/(octet-stream|zip|gzip|pdf)/.test(contentType)) return null;
  const charset = contentType.match(/charset\s*=\s*["']?([^;\s"']+)/)?.[1] || 'utf-8';
  try {
    const text = new TextDecoder(charset, { fatal: true, ignoreBOM: true }).decode(await blob.arrayBuffer());
    return /[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/.test(text) ? null : text;
  } catch { return null; }
}
