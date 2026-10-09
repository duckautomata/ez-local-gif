// Friendly labels for the render job stages (Go: jobs.Stage*), shared by
// the Render panel and the batch rows. An unknown stage shows as its id, so
// a newer server's stage is never hidden.

const STAGE_LABELS: Record<string, string> = {
  probe: 'Probing source',
  // Phase 5b: the AI matte pass before the master; the job message carries "24/45 · GPU" / "loading model (12 s)"
  matte: 'AI matte',
  master: 'Decoding frames',
  encode: 'Encoding',
  fit: 'Fitting to size',
  lint: 'Discord lint',
  verify: 'Verifying',
  done: 'Done',
};

/** stageLabel returns the label of a job stage ('' for none, the id itself when unknown). */
export function stageLabel(stage: string | null | undefined): string {
  if (!stage) return '';
  return STAGE_LABELS[stage] ?? stage;
}
