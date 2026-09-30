// Index signatures intentionally retain server fields added by newer backends.
export interface Upstream {
  [key: string]: unknown;
  id: string;
  name: string;
  base_url: string;
  enabled: boolean;
  weight: number;
  timeout_ms: number;
  path_globs: string[] | null;
  model_globs: string[] | null;
  headers: Record<string, string> | null;
}

export interface RetryConfig {
  [key: string]: unknown;
  max_attempts: number;
  base_delay_ms: number;
  max_delay_ms: number;
  jitter: boolean;
  retry_on_network_error: boolean;
  status_codes: number[] | null;
  body_regexes: string[] | null;
}

export interface Config {
  [key: string]: unknown;
  listen: string;
  log_limit: number;
  max_request_body_bytes: number;
  max_buffer_bytes: number;
  max_log_body_bytes: number;
  body_sniff_timeout_ms?: number;
  error_body_timeout_ms?: number;
  upstreams: Upstream[] | null;
  retry: RetryConfig;
}

export interface Stats {
  total_requests: number;
  retried_requests: number;
  retry_success: number;
  failed_requests: number;
  uptime_seconds: number;
}

export interface ResponseBodyCapture {
  response_body_bytes?: number;
  response_body_captured_bytes?: number;
  response_body_complete?: boolean;
  response_body_available?: boolean;
  response_body_error?: string;
}

export interface Attempt extends ResponseBodyCapture {
  [key: string]: unknown;
  upstream_id: string;
  upstream_name: string;
  url: string;
  status: number;
  duration_ms: number;
  error: string;
  retry_reason: string;
  response_body: string;
  response_body_truncated: boolean;
  response_headers?: Record<string, string[]> | null;
  regex_skipped?: boolean;
}

export interface LogEntry extends ResponseBodyCapture {
  [key: string]: unknown;
  id: string;
  time: string;
  method: string;
  path: string;
  query: string;
  model: string;
  client_ip: string;
  status: number;
  duration_ms: number;
  attempt_count: number;
  retried: boolean;
  upstream_id: string;
  upstream_name: string;
  request_headers: Record<string, string[]> | null;
  request_body: string;
  request_body_truncated: boolean;
  request_body_bytes?: number;
  request_body_captured_bytes?: number;
  request_body_complete?: boolean;
  request_body_available?: boolean;
  request_body_error?: string;
  response_headers: Record<string, string[]> | null;
  response_body: string;
  response_body_truncated: boolean;
  attempts: Attempt[] | null;
  streaming?: boolean;
  regex_skipped?: boolean;
  incomplete?: boolean;
  error?: string;
  response_body_part?: string;
}

export interface LogsResponse { logs: LogEntry[] | null; total: number }
export type Page = 'overview' | 'upstreams' | 'retry' | 'logs' | 'settings';
