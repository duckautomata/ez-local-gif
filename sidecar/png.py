"""Minimal 8-bit grayscale PNG codec on zlib + struct (no third-party code).

The sidecar answers every frame with one size x size 8-bit gray PNG and the
app side resizes it with ffmpeg, so a writer for exactly that shape is all the
service needs — not an imaging library (cv2 is 50 MB and carries an LGPL
FFmpeg notice, Pillow another 3 MB; both would be used for one function).

``encode_gray`` writes a single IDAT with filter type 0 (None) on every row:
a matte is mostly flat 0 / 255 areas with a soft rim and zlib compresses those
well without prediction (~5–40 KB for a 1024² matte). ``decode_gray`` reads
back non-interlaced 8-bit grayscale PNGs with any of the five standard filter
types; it exists for the self-test and the pytest suite, the app never decodes
a matte in Python.
"""
from __future__ import annotations

import struct
import zlib

import numpy as np

SIGNATURE = b"\x89PNG\r\n\x1a\n"
COMPRESSION_LEVEL = 4  # zlib level: a 1024² matte in ~5 ms; the memo size is a few KB either way


def _chunk(tag: bytes, data: bytes) -> bytes:
    return struct.pack(">I", len(data)) + tag + data + struct.pack(">I", zlib.crc32(tag + data) & 0xFFFFFFFF)


def encode_gray(img, level: int = COMPRESSION_LEVEL) -> bytes:
    """Encode a 2-D uint8 array (H x W) as an 8-bit grayscale PNG."""
    a = np.asarray(img)
    if a.ndim != 2:
        raise ValueError(f"encode_gray: expected a 2-D array, got shape {a.shape}")
    if a.dtype != np.uint8:
        raise ValueError(f"encode_gray: expected uint8, got {a.dtype}")
    h, w = a.shape
    if h < 1 or w < 1:
        raise ValueError("encode_gray: empty image")
    rows = np.empty((h, w + 1), dtype=np.uint8)
    rows[:, 0] = 0  # filter type 0 (None) on every row
    rows[:, 1:] = a
    ihdr = struct.pack(">IIBBBBB", w, h, 8, 0, 0, 0, 0)  # 8-bit, colour type 0 (gray), deflate, no interlace
    return SIGNATURE + _chunk(b"IHDR", ihdr) + _chunk(b"IDAT", zlib.compress(rows.tobytes(), level)) + _chunk(b"IEND", b"")


def _paeth(a: int, b: int, c: int) -> int:
    p = a + b - c
    pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
    if pa <= pb and pa <= pc:
        return a
    return b if pb <= pc else c


def decode_gray(data: bytes) -> np.ndarray:
    """Decode a non-interlaced 8-bit grayscale PNG into a uint8 array (H x W)."""
    if not data.startswith(SIGNATURE):
        raise ValueError("decode_gray: not a PNG")
    pos = len(SIGNATURE)
    width = height = None
    idat = []
    while pos + 8 <= len(data):
        (length,) = struct.unpack(">I", data[pos : pos + 4])
        tag = data[pos + 4 : pos + 8]
        body = data[pos + 8 : pos + 8 + length]
        if len(body) != length:
            raise ValueError("decode_gray: truncated chunk")
        pos += 12 + length
        if tag == b"IHDR":
            width, height, depth, ctype, _, _, interlace = struct.unpack(">IIBBBBB", body)
            if depth != 8 or ctype != 0 or interlace != 0:
                raise ValueError(f"decode_gray: unsupported PNG (depth {depth}, colour type {ctype}, interlace {interlace})")
        elif tag == b"IDAT":
            idat.append(body)
        elif tag == b"IEND":
            break
    if width is None or not idat:
        raise ValueError("decode_gray: missing IHDR or IDAT")
    raw = np.frombuffer(zlib.decompress(b"".join(idat)), dtype=np.uint8)
    if raw.size != height * (width + 1):
        raise ValueError("decode_gray: wrong decompressed size")
    raw = raw.reshape(height, width + 1)
    out = np.zeros((height, width), dtype=np.uint8)
    zero = np.zeros(width, dtype=np.uint8)
    for y in range(height):
        f, line = int(raw[y, 0]), raw[y, 1:]
        up = out[y - 1] if y else zero
        if f == 0:
            out[y] = line
        elif f == 1:  # Sub: sequential dependency along the row
            acc = 0
            row = out[y]
            for x in range(width):
                acc = (int(line[x]) + acc) & 0xFF
                row[x] = acc
        elif f == 2:
            out[y] = (line.astype(np.uint16) + up) & 0xFF
        elif f == 3:
            row = out[y]
            left = 0
            for x in range(width):
                left = (int(line[x]) + ((left + int(up[x])) >> 1)) & 0xFF
                row[x] = left
        elif f == 4:
            row = out[y]
            left = 0
            upleft = 0
            for x in range(width):
                left = (int(line[x]) + _paeth(left, int(up[x]), upleft)) & 0xFF
                row[x] = left
                upleft = int(up[x])
        else:
            raise ValueError(f"decode_gray: bad filter type {f}")
    return out
