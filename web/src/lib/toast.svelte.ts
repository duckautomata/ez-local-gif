// Minimal toast store: errors stay 10 s, info 5 s, click to dismiss.

export type ToastKind = 'error' | 'info' | 'success';

export interface Toast {
  id: number;
  kind: ToastKind;
  text: string;
}

export const toasts = $state<Toast[]>([]);

let nextId = 1;

function push(kind: ToastKind, text: string, ttl: number): void {
  // Collapse exact duplicates (e.g. repeated "server offline").
  const dup = toasts.find((t) => t.kind === kind && t.text === text);
  if (dup) dismiss(dup.id);
  const id = nextId++;
  toasts.push({ id, kind, text });
  window.setTimeout(() => dismiss(id), ttl);
}

export function dismiss(id: number): void {
  const i = toasts.findIndex((t) => t.id === id);
  if (i >= 0) toasts.splice(i, 1);
}

export const toast = {
  error: (text: string) => push('error', text, 10_000),
  info: (text: string) => push('info', text, 5_000),
  success: (text: string) => push('success', text, 4_000),
};
