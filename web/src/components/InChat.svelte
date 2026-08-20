<script lang="ts">
  // "As seen in chat": the rendered file at the size Discord displays it —
  // emoji inline (22 px) and jumbo (48 px), stickers at 160 px — on both the
  // dark and the light theme, so 1-bit GIF edges can be judged before upload.
  import type { ResultFile } from '../lib/api';
  import { chatSizes } from '../lib/result';

  interface Props {
    file: ResultFile;
    target: string;
  }
  let { file, target }: Props = $props();

  const sizes = $derived(chatSizes(target));
  const themes = [
    { id: 'dark', label: 'dark', cls: 'theme-dark' },
    { id: 'light', label: 'light', cls: 'theme-light' },
  ] as const;
</script>

{#if sizes.length}
  <div class="inchat">
    <span class="hint">As seen in chat:</span>
    {#each themes as th (th.id)}
      <div class="tile {th.cls}" title="Discord {th.label} theme">
        {#each sizes as s (s.px)}
          {#if target === 'emote'}
            <span class="line" style:font-size="{s.px === 48 ? 22 : 15}px">
              {#if s.px === 48}
                <img src={file.url} alt="{s.label} preview" style:height="{s.px}px" style:width="{s.px}px" />
              {:else}
                gg <img src={file.url} alt="{s.label} preview" style:height="{s.px}px" style:width="{s.px}px" /> nice
              {/if}
              <span class="cap">{s.label}</span>
            </span>
          {:else}
            <span class="line">
              <img src={file.url} alt="{s.label} preview" style:height="{s.px}px" style:width="{s.px}px" />
              <span class="cap">{s.label}</span>
            </span>
          {/if}
        {/each}
      </div>
    {/each}
  </div>
{/if}

<style>
  .inchat {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px 12px;
  }
  .tile {
    display: inline-flex;
    align-items: center;
    gap: 14px;
    padding: 8px 12px;
    border-radius: var(--radius-sm);
    border: 1px solid var(--border);
    font-family: var(--font);
  }
  .theme-dark {
    background: #313338;
    color: #dbdee1;
  }
  .theme-light {
    background: #ffffff;
    color: #313338;
  }
  .line {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    line-height: 1;
  }
  img {
    object-fit: contain;
    vertical-align: middle;
    image-rendering: auto;
  }
  .cap {
    font-size: 10.5px;
    opacity: 0.6;
    margin-left: 4px;
    white-space: nowrap;
  }
</style>
