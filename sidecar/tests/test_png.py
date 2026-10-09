"""png.py: the gray PNG writer round-trips, and the reader handles every standard filter type (for the tests' own use)."""
from __future__ import annotations

import struct
import zlib

import numpy as np
import pytest

from conftest import png


@pytest.mark.parametrize("shape", [(1, 1), (8, 8), (17, 5), (64, 3), (1024, 1024)])
def test_round_trip(shape):
    rng = np.random.default_rng(shape[0] * 131 + shape[1])
    a = rng.integers(0, 256, shape, dtype=np.uint8)
    data = png.encode_gray(a)
    assert data.startswith(b"\x89PNG\r\n\x1a\n")
    w, h, depth, ctype = struct.unpack(">IIBB", data[16:26])
    assert (w, h, depth, ctype) == (shape[1], shape[0], 8, 0)
    b = png.decode_gray(data)
    assert b.dtype == np.uint8 and np.array_equal(a, b)


def test_flat_matte_is_small():
    a = np.zeros((1024, 1024), np.uint8)
    a[200:700, 300:800] = 255
    assert len(png.encode_gray(a)) < 8000


def test_rejects_wrong_input():
    with pytest.raises(ValueError):
        png.encode_gray(np.zeros((4, 4, 3), np.uint8))
    with pytest.raises(ValueError):
        png.encode_gray(np.zeros((4, 4), np.float32))
    with pytest.raises(ValueError):
        png.decode_gray(b"not a png")


def _filtered_png(img: np.ndarray, ftype: int) -> bytes:
    """Reference encoder applying one PNG filter type to every row (PNG spec 9.2)."""
    h, w = img.shape
    rows = []
    prev = np.zeros(w, np.int16)
    for y in range(h):
        cur = img[y].astype(np.int16)
        left = np.concatenate([[0], cur[:-1]])
        upleft = np.concatenate([[0], prev[:-1]])
        if ftype == 0:
            f = cur
        elif ftype == 1:
            f = cur - left
        elif ftype == 2:
            f = cur - prev
        elif ftype == 3:
            f = cur - ((left + prev) >> 1)
        else:
            p = left + prev - upleft
            pa, pb, pc = np.abs(p - left), np.abs(p - prev), np.abs(p - upleft)
            pred = np.where((pa <= pb) & (pa <= pc), left, np.where(pb <= pc, prev, upleft))
            f = cur - pred
        rows.append(bytes([ftype]) + (f & 0xFF).astype(np.uint8).tobytes())
        prev = cur
    raw = b"".join(rows)

    def chunk(tag, data):
        return struct.pack(">I", len(data)) + tag + data + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)

    return png.SIGNATURE + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 0, 0, 0, 0)) + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b"")


@pytest.mark.parametrize("ftype", [0, 1, 2, 3, 4])
def test_decoder_handles_every_filter_type(ftype):
    rng = np.random.default_rng(ftype)
    img = rng.integers(0, 256, (13, 9), dtype=np.uint8)
    assert np.array_equal(png.decode_gray(_filtered_png(img, ftype)), img)
