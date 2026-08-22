# Frontend phase 1 review

Bugs are must fix. Improvements are nice to have.

## Bugs
1. When discord target or output is selected to be Sticker, the requirement `gif.sticker-dims` shows as an error if the output dimensions are not exactly 320x320 `400x400; stickers must be exactly 320x320`. This is not an error - like emote - Discord will shrink any sticker down to 320x320, accepts files under 320x320, and does not have to be square. I would suggest treating it the same as emote - have it be a warning if one of the dim is larger than 320.


## Improvements
1. For the preview, it would be nice to show the playback in terms of frames. If it's a video then seconds+frame. Sorta of like how Davinci Resolve does it.
2. For the preview, add a next-frame, previous-frame button/keyboard action (when focused in the preview window). This would be a lot easier than having to scrub.
3. In the result, it would be nice to add a "edit as source" which will open in a new tab with the rendered result as the source input.