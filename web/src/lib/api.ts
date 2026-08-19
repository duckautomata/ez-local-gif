// HTTP client for the ez-local-gif backend and the TypeScript mirror of its
// JSON shapes. Field names follow the Go `json:"..."` tags exactly:
//
//   internal/recipe/recipe.go        ProbeInfo, Source, Op(+*Params), Output, Recipe
//   internal/jobs/jobs.go            File (here: ResultFile), Result, Job, Event (here: JobEvent)
//   internal/discordlint/discordlint.go  Report, Check
//
// `File` and `Event` are renamed on the TS side only to avoid shadowing the
// DOM globals of the same name; their JSON is unchanged.
//
// API (see internal/server/server.go):
//   POST /api/upload            multipart "file"      -> Source
//   GET  /api/sources/{hash}                          -> Source
//   POST /api/still             StillRequest          -> image/png
//   POST /api/jobs              Recipe                -> 202 Job
//   GET  /api/jobs/{id}                               -> Job
//   DELETE /api/jobs/{id}                             -> 204
//   GET  /api/jobs/{id}/events  text/event-stream of JobEvent
//   GET  /api/results/{hash}                          -> Result
//   GET  /out/{hash}/{name}[?dl=1]                    result file
//   GET  /api/capabilities                            -> Capabilities
//   GET  /healthz                                     "ok"
// Errors are {"error": "message"} with a 4xx/5xx status.

// ---------------------------------------------------------------------------
// recipe

export type Kind = 'video' | 'animation' | 'image' | 'sequence';

export interface ProbeInfo {
  format: string;
  codec: string;
  profile?: string;
  pixFmt: string;
  bits: number;
  width: number;
  height: number;
  fps: number;
  duration: number;
  frames: number;
  hasAlpha: boolean;
  hasAudio: boolean;
  isStill: boolean;
  kind: Kind;
  premultiplied: boolean;
}

export interface Source {
  hash: string;
  name: string;
  size: number;
  info: ProbeInfo;
}

export type OpKind =
  | 'trim'
  | 'crop'
  | 'resize'
  | 'canvas'
  | 'fps'
  | 'speed'
  | 'flip'
  | 'rotate'
  | 'unpremultiply';

export type FitMode = 'contain' | 'cover' | 'exact';

export interface TrimParams {
  start: number;
  end?: number; // <= 0 / omitted = to the end
}
export interface CropParams {
  x: number;
  y: number;
  w: number;
  h: number;
}
export interface ResizeParams {
  width?: number;
  height?: number;
  fit?: FitMode;
}
export interface CanvasParams {
  width: number;
  height: number;
  color?: string;
}
export interface FPSParams {
  fps: number;
}
export interface SpeedParams {
  factor: number;
}
export interface FlipParams {
  horizontal?: boolean;
  vertical?: boolean;
}
export interface RotateParams {
  degrees: 90 | 180 | 270;
}

export type OpParams =
  | TrimParams
  | CropParams
  | ResizeParams
  | CanvasParams
  | FPSParams
  | SpeedParams
  | FlipParams
  | RotateParams;

/** One step of the non-destructive op stack (Go: recipe.Op; params is json.RawMessage). */
export interface Op {
  kind: OpKind;
  params?: OpParams;
}

export type OutputFormat = 'gif' | 'webp';
export type Target = '' | 'emote' | 'sticker' | 'attachment';
export type PresetId = 'emote' | 'sticker' | 'chat-gif' | 'chat-webp' | 'custom';
export type Dither = 'bayer' | 'sierra2_4a' | 'floyd_steinberg' | 'none';

/** Go: recipe.Output. Zero values / omitted fields mean "default". */
export interface Output {
  format: OutputFormat;
  width?: number;
  height?: number;
  fit?: FitMode;
  fps?: number;
  quality?: number;
  lossless?: boolean;
  lossy?: number;
  colors?: number;
  dither?: Dither;
  alphaThreshold?: number;
  matte?: string;
  loop?: number;
  fitBytes?: number;
  preset?: PresetId;
  target?: Target;
}

export interface Recipe {
  v: number;
  sources: string[];
  ops: Op[];
  output: Output;
}

export const RECIPE_VERSION = 1;

// ---------------------------------------------------------------------------
// discordlint

export type Level = 'error' | 'warn' | 'info';

export interface Check {
  rule: string;
  level: Level;
  ok: boolean;
  fixed: boolean;
  detail: string;
}

export interface Report {
  rulesVersion: string;
  format: string;
  target: Target;
  bytes: number;
  limit: number;
  width: number;
  height: number;
  frames: number;
  durationMs: number;
  minDelayMs: number;
  loopForever: boolean;
  hasAlpha: boolean;
  checks: Check[] | null;
  ok: boolean;
}

// ---------------------------------------------------------------------------
// jobs

export type JobState = 'queued' | 'running' | 'done' | 'error';
export type Stage = 'probe' | 'master' | 'encode' | 'lint' | 'verify' | 'done' | (string & {});

/** Go: jobs.File — one produced output. */
export interface ResultFile {
  name: string;
  url: string;
  format: string;
  bytes: number;
  width: number;
  height: number;
  frames: number;
  fps: number;
  duration: number;
  limit: number;
  report?: Report | null;
}

/** Go: jobs.Result — the manifest written to the result dir. */
export interface Result {
  recipeHash: string;
  recipe: Recipe;
  files: ResultFile[] | null;
  created: string;
  renderMs: number;
  cached: boolean;
  tools?: Record<string, string>;
}

/** Go: jobs.Job (snapshot). Times are RFC 3339 strings. */
export interface Job {
  id: string;
  recipeHash: string;
  recipe: Recipe;
  state: JobState;
  stage: Stage;
  percent: number;
  message: string;
  result?: Result | null;
  error?: string;
  created: string;
  started?: string;
  finished?: string;
}

/** Go: jobs.Event — SSE payload. */
export interface JobEvent {
  type: 'progress' | 'done' | 'error';
  job: Job;
}

// ---------------------------------------------------------------------------
// misc endpoints

export interface StillRequest {
  src: string;
  ops: Op[];
  output: Output;
  t: number;
  maxW: number;
}

export interface Capabilities {
  tools: Record<string, string>;
  limits: Record<string, unknown>;
  rulesVersion: string;
}

// ---------------------------------------------------------------------------
// errors

export const OFFLINE_MESSAGE = 'Cannot reach the ez-local-gif server. Is `ezlg serve` running on :8080?';

/** ApiError carries the server's error text (or a network explanation) plus the HTTP status (0 = network). */
export class ApiError extends Error {
  readonly status: number;
  constructor(message: string, status: number) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

export function isAbortError(e: unknown): boolean {
  return e instanceof DOMException && e.name === 'AbortError';
}

/** messageOf turns any thrown value into a human-readable string. */
export function messageOf(e: unknown): string {
  if (e instanceof Error) return e.message || e.name;
  return String(e);
}

/** errorFromBody extracts {"error": "..."} from a response body, falling back to the raw text or status. */
function errorFromBody(body: string, status: number, statusText: string): string {
  const text = body.trim();
  if (text) {
    try {
      const j: unknown = JSON.parse(text);
      if (j && typeof j === 'object' && typeof (j as { error?: unknown }).error === 'string') {
        return (j as { error: string }).error;
      }
    } catch {
      // not JSON — use raw text below
    }
    // Avoid dumping a whole HTML page (e.g. a proxy error) into a toast.
    if (!/^\s*</.test(text)) return text.length > 500 ? `${text.slice(0, 500)}…` : text;
  }
  const http = `HTTP ${status}${statusText ? ` ${statusText}` : ''}`;
  // A reverse proxy (or the Vite dev server) answering for a dead backend.
  if (status === 502 || status === 503 || status === 504) return `${OFFLINE_MESSAGE} (${http})`;
  return http;
}

async function request(url: string, init: RequestInit = {}): Promise<Response> {
  let res: Response;
  try {
    res = await fetch(url, init);
  } catch (e) {
    if (isAbortError(e)) throw e;
    throw new ApiError(OFFLINE_MESSAGE, 0);
  }
  if (!res.ok) {
    const body = await res.text().catch(() => '');
    throw new ApiError(errorFromBody(body, res.status, res.statusText), res.status);
  }
  return res;
}

async function requestJSON<T>(url: string, init: RequestInit = {}): Promise<T> {
  const res = await request(url, init);
  try {
    return (await res.json()) as T;
  } catch {
    throw new ApiError(`Malformed JSON from ${url}`, res.status);
  }
}

function jsonInit(method: string, body: unknown, signal?: AbortSignal): RequestInit {
  return {
    method,
    headers: { 'Content-Type': 'application/json', Accept: 'application/json, image/png' },
    body: JSON.stringify(body),
    signal,
  };
}

// ---------------------------------------------------------------------------
// endpoints

export interface UploadHandle {
  promise: Promise<Source>;
  abort(): void;
}

/**
 * upload streams one file as multipart/form-data (field "file") with XHR so
 * upload progress is observable. Resolves with the probed Source.
 */
export function upload(file: File, onProgress?: (loaded: number, total: number) => void): UploadHandle {
  const xhr = new XMLHttpRequest();
  const promise = new Promise<Source>((resolve, reject) => {
    xhr.open('POST', '/api/upload');
    xhr.responseType = 'text';
    xhr.setRequestHeader('Accept', 'application/json');
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onProgress?.(e.loaded, e.total);
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) {
        try {
          resolve(JSON.parse(xhr.responseText) as Source);
        } catch {
          reject(new ApiError('Upload succeeded but the server response was not JSON', xhr.status));
        }
        return;
      }
      reject(new ApiError(errorFromBody(xhr.responseText, xhr.status, xhr.statusText), xhr.status));
    };
    xhr.onerror = () => reject(new ApiError(OFFLINE_MESSAGE, 0));
    xhr.ontimeout = () => reject(new ApiError('Upload timed out', 0));
    xhr.onabort = () => reject(new DOMException('Upload cancelled', 'AbortError'));
    const form = new FormData();
    form.append('file', file, file.name);
    xhr.send(form);
  });
  return { promise, abort: () => xhr.abort() };
}

export function getSource(hash: string, signal?: AbortSignal): Promise<Source> {
  return requestJSON<Source>(`/api/sources/${encodeURIComponent(hash)}`, { signal });
}

/** fetchStill renders one preview frame (PNG) for the given op stack at output time t. */
export async function fetchStill(req: StillRequest, signal?: AbortSignal): Promise<Blob> {
  const res = await request('/api/still', jsonInit('POST', req, signal));
  return res.blob();
}

export function submitJob(recipe: Recipe, signal?: AbortSignal): Promise<Job> {
  return requestJSON<Job>('/api/jobs', jsonInit('POST', recipe, signal));
}

export function getJob(id: string, signal?: AbortSignal): Promise<Job> {
  return requestJSON<Job>(`/api/jobs/${encodeURIComponent(id)}`, { signal });
}

export async function cancelJob(id: string): Promise<void> {
  await request(`/api/jobs/${encodeURIComponent(id)}`, { method: 'DELETE' });
}

export function getResult(recipeHash: string, signal?: AbortSignal): Promise<Result> {
  return requestJSON<Result>(`/api/results/${encodeURIComponent(recipeHash)}`, { signal });
}

export function getCapabilities(signal?: AbortSignal): Promise<Capabilities> {
  return requestJSON<Capabilities>('/api/capabilities', { signal });
}

/** ping resolves true when /healthz answers. Never throws. */
export async function ping(signal?: AbortSignal): Promise<boolean> {
  try {
    const res = await fetch('/healthz', { signal, cache: 'no-store' });
    return res.ok;
  } catch {
    return false;
  }
}

/** downloadURL adds ?dl=1 so the server sends Content-Disposition: attachment. */
export function downloadURL(fileURL: string): string {
  return fileURL + (fileURL.includes('?') ? '&' : '?') + 'dl=1';
}

// ---------------------------------------------------------------------------
// job progress

export interface JobWatcher {
  progress?: (job: Job) => void;
  done: (job: Job) => void;
  error: (message: string, job?: Job) => void;
}

/**
 * watchJob subscribes to /api/jobs/{id}/events (SSE) and calls back on every
 * snapshot. If the event stream cannot be opened or drops, it degrades to
 * polling GET /api/jobs/{id} once a second. Returns a stop function; the
 * watcher also stops itself after done/error.
 */
export function watchJob(id: string, on: JobWatcher): () => void {
  let stopped = false;
  let es: EventSource | null = null;
  let pollTimer: number | undefined;

  const stop = () => {
    stopped = true;
    if (pollTimer !== undefined) window.clearTimeout(pollTimer);
    pollTimer = undefined;
    es?.close();
    es = null;
  };

  const handle = (job: Job) => {
    if (stopped) return;
    if (job.state === 'done') {
      stop();
      on.done(job);
    } else if (job.state === 'error') {
      stop();
      on.error(job.error || 'Render failed', job);
    } else {
      on.progress?.(job);
    }
  };

  const parse = (data: string): Job | null => {
    try {
      const v = JSON.parse(data) as JobEvent | Job;
      if (v && typeof v === 'object' && 'job' in v) return v.job;
      if (v && typeof v === 'object' && 'id' in v) return v as Job;
    } catch {
      // ignore malformed frames; the next one (or polling) will catch up
    }
    return null;
  };

  const poll = async () => {
    if (stopped) return;
    try {
      handle(await getJob(id));
    } catch (e) {
      if (!stopped) {
        stop();
        on.error(messageOf(e));
      }
      return;
    }
    if (!stopped) pollTimer = window.setTimeout(() => void poll(), 1000);
  };

  if (typeof EventSource === 'undefined') {
    void poll();
    return stop;
  }

  try {
    es = new EventSource(`/api/jobs/${encodeURIComponent(id)}/events`);
  } catch {
    void poll();
    return stop;
  }

  const onMessage = (e: globalThis.Event) => {
    const data = (e as MessageEvent<unknown>).data;
    if (typeof data !== 'string') return;
    const job = parse(data);
    if (job) handle(job);
  };
  for (const type of ['progress', 'done', 'error', 'message']) es.addEventListener(type, onMessage);
  // A server-sent "event: error" also reaches onerror (as a MessageEvent with
  // data) — that case is handled above. A bare Event here means the
  // connection failed or dropped: fall back to polling.
  es.onerror = (e) => {
    if (stopped) return;
    if ((e as MessageEvent<unknown>).data !== undefined) return;
    es?.close();
    es = null;
    void poll();
  };
  return stop;
}
