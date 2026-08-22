## Bugs
1. The preview player scrubber, when you scroll to the end manually, you go 1 frame further than what the max frame counter shows, but it doesn't move past the last frame. Not a major issue, but can cause confusion on why manually scrubbing the last two spots do nothing (since they are the same frame.)
2. The preview player scrubber doesn't always go to the very end of the bar. It usually leaves a gap. Same as above, not major but still annoying.
3. Because of (1.), when I scroll to the end of the clip, then open Trim and set the start to be "from scrubber", I get a Preeview failed" error `jobs: invalid recipe: graph: trim start (4.22 s) is at or beyond the end of the source (4.217 s)`
4. When using a sequence, under Delay, changing the delay (fps) sometimes changes the total frames. For example, when I import it as 100ms delay, then change it later to be 33ms delay, it went from 34 frames to 33. Also, when I did this, scrubbing now sometimes jumps 2 frames and sometimes jumps none. Note that this not effect the output. Just the scrubbing preview.

## UI/UX Improvements
1. Under Crop, the "Full frame" button essentially acts as a clear/reset button. Rename it to be more clear.
2. Under Output, when selected Chat Gif/webp/avif, I am unable to modify the Discord target of free tier.
3. The entire Output section is kind of cluttered and clunky. With Format and Output kind of being duplicates (especially for Chat gif/webp/avif options being the same as changing the format dropdown). It is all around confusing. Try to improve it. This probably will fix (2.)
4. Have the GIF icon on the top left be clickable. Clicking on it will take you back to the landing page. Easy way to restart from empty.
5. For the sequence delay during sequence input, I'd recommend displaying what the fps will be with the given ms delay.

## Questions
1. when canceling a render, does it actually cancel on the server? Say, if I add a 1 GiB massive video file and accidentally hit render before trimming it, and hit cancel, will it cause the entire server to hang while we wait?