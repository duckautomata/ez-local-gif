"""Example runner: naive RGB colour key.

The key colour is the pixel at (row 2, col 2) of each frame. alpha = 0 where
the Euclidean RGB distance to the key is below THRESHOLD, 1 above
THRESHOLD + BLEND, linear in between (a crude stand-in for ffmpeg's
``colorkey=similarity:blend``). Pure numpy, device is ignored.
"""
import numpy as np

NAME = "numpy_colorkey"
BATCH = 1
THRESHOLD = 60.0  # RGB distance (0..441) treated as background
BLEND = 30.0      # width of the soft ramp above THRESHOLD
KEY_PIXEL = (2, 2)  # (row, col)


def load(device: str):
    return {"threshold": THRESHOLD, "blend": BLEND, "key_pixel": KEY_PIXEL}


def key_frame(frame: np.ndarray, threshold: float, blend: float, key_pixel) -> np.ndarray:
    key = frame[key_pixel[0], key_pixel[1]].astype(np.float32)
    d = np.sqrt(((frame.astype(np.float32) - key) ** 2).sum(axis=2))
    if blend <= 0:
        return (d > threshold).astype(np.float32)
    return np.clip((d - threshold) / blend, 0.0, 1.0).astype(np.float32)


def infer(ctx, frames):
    return [key_frame(f, ctx["threshold"], ctx["blend"], ctx["key_pixel"]) for f in frames]
