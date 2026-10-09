"""Example runner: constant 0.5 matte (a floor for every metric)."""
import numpy as np

NAME = "dummy"
BATCH = 4


def load(device: str):
    return {"device": device}


def infer(ctx, frames):
    return [np.full(f.shape[:2], 0.5, dtype=np.float32) for f in frames]
