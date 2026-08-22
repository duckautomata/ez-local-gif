# Discord render-test result matrix

### Results
| file | attachment | sticker | emote | notes |
|---|---|---|---|---|
| a_gif_ffmpeg-palette_gifsicle-O2 | good |  |  | as expected |
| b_gif_ffmpeg-palette_gifsicle-U | good |  |  | as expected |
| c_gif_gifski_local-palettes | not transparent, has a dark-gray background |  |  | leaves a ghosting effect of the moving circle as the gif plays |
| d_gif_ffmpeg-only | good |  |  | as expected. transparent and no flicker |
| e_webp_lossy_yuva420p_q80 | good |  |  | as expected, the q80 does result in the square grid noise being blurry. |
| e2_webp_lossy_bgra_q80 | good |  |  | same as (e), file size is slightly larger (21KB). |
| f_webp_lossless_bgra | good |  |  | as expected, the noise in the square grid is now clear |
| g_apng_rgba | shows frame 0 clear |  |  | as expected, Downloading the attachment no longer is animated. |
| h1_emote128_gif |  |  | slight bugged, the top right square has two white lines going through it. | as expected. |
| h2_emote128_webp |  |  | good | as expected |
| i1_sticker320_gif |  | good |  | as expected |
| i2_sticker320_apng-rgba |  | good |  | runs at the 5fps, no error shown when uploading |
| i3_sticker320_apng-indexed |  | good |  | as expected, same as (i2) but now at 25fps. |
| j_avif_alpha | good |  |  | animated with soft alpha, looks good. |

## Additional Notes
- All the gif files had a dark outlines visible on light mode and no smooth alpha.
- APNG files were by far the best in terms of sticker quality. It was useless everywhere else.