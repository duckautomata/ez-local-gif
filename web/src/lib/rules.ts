// Friendly labels for the discordlint rule ids (internal/discordlint gif.go,
// webp.go, apng.go; render.alpha comes from internal/jobs). The linter's
// detail text carries the specifics; the label says what the rule is about
// in plain words. Unknown ids fall back to the id itself, so a new rule is
// never hidden.

const LABELS: Record<string, string> = {
  // GIF
  'gif.gce-every-frame': 'Graphic Control Extension on every frame',
  'gif.frame0-transparency': 'First frame declares transparency',
  'gif.lsd-background-index': 'Screen background index matches transparency',
  'gif.disposal': 'Explicit frame disposal (1 or 2)',
  'gif.netscape-loop': 'NETSCAPE loop block',
  'gif.min-delay': 'Frame delays ≥ 2 cs',
  'gif.global-palette': 'Single global palette',
  'gif.no-interlace': 'Not interlaced',
  'gif.no-extra-extensions': 'No comment / text / app extensions',
  'gif.first-frame-visible': 'First frame is not blank',
  'gif.trailer': 'File trailer present',
  'gif.size-limit': 'Within the byte limit',
  'gif.sticker-dims': 'Sticker size 320×320',
  'gif.sticker-duration': 'Sticker ≤ 5 s / 1000 frames / 60 fps',
  'gif.emote-dims': 'Emote size 128×128',
  // WebP
  'webp.riff': 'Valid RIFF / WebP container',
  'webp.anim-flag': 'ANIM chunk matches frame count',
  'webp.alpha-flag': 'VP8X alpha flag matches the frames',
  'webp.loop-forever': 'Loops forever',
  'webp.canvas': 'Canvas equals the frame size',
  'webp.no-metadata': 'No EXIF / XMP / ICC metadata',
  'webp.min-delay': 'Frame delays ≥ 20 ms',
  'webp.size-limit': 'Within the byte limit',
  'webp.sticker': 'WebP is not a sticker format',
  'webp.emote-dims': 'Emote size 128×128',
  // APNG
  'apng.container': 'Valid PNG / APNG container',
  'apng.plays-forever': 'Plays forever (num_plays 0)',
  'apng.first-frame': 'First frame is part of the animation',
  'apng.canvas': 'Every frame inside the canvas',
  'apng.min-delay': 'Frame delays ≥ 20 ms, never 0',
  'apng.size-limit': 'Within the byte limit',
  'apng.sticker': 'Sticker limits (320×320, ≤ 5 s, ≤ 1000 frames, ≤ 60 fps)',
  'apng.not-emote': 'APNG cannot be an animated emote',
  'apng.attachment': 'APNG attachments show only frame 0',
  // Reads correctly for both outcomes: ✓ when the output is indexed, and a
  // neutral info dot when an RGBA-truecolour output fails the check.
  'apng.indexed': 'Indexed 8-bit-alpha APNG (sticker default rung)',
  // static images
  'static.size-limit': 'Within the byte limit',
  'static.emote-dims': 'Emote size 128×128',
  'static.sticker': 'Sticker format / size',
  'static.format': 'Image summary',
  // render pipeline / fit engine (internal/jobs)
  'render.alpha': 'Transparency as rendered',
  'fit.target': 'Fit-to-size budget reached',
};

/** ruleLabel returns the friendly label for a discordlint rule id (the id itself when unknown). */
export function ruleLabel(rule: string): string {
  return LABELS[rule] ?? rule;
}

/** knownRules lists every rule id with a label (for tests and docs). */
export function knownRules(): string[] {
  return Object.keys(LABELS);
}
