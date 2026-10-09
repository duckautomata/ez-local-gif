"""Print the models.json facts (bytes, sha256) of a model file or URL.

Usage:  python -I tools/pin-model.py <path-or-https-url> [...]

Downloads a URL to memory-less streaming (nothing is kept), hashes it and
prints a JSON fragment to paste into models.json. Pin by sha256, never by
URL alone: the app's memo keys fold the sha256 in as `weights`.
"""
from __future__ import annotations

import hashlib
import json
import sys
import urllib.request


def pin(src: str) -> dict:
    h = hashlib.sha256()
    n = 0
    if src.startswith(("http://", "https://")):
        with urllib.request.urlopen(src, timeout=60) as r:  # noqa: S310 - the operator's own URL
            for chunk in iter(lambda: r.read(1 << 20), b""):
                h.update(chunk)
                n += len(chunk)
    else:
        with open(src, "rb") as f:
            for chunk in iter(lambda: f.read(1 << 20), b""):
                h.update(chunk)
                n += len(chunk)
    return {"source": src, "bytes": n, "sha256": h.hexdigest()}


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    for arg in sys.argv[1:]:
        print(json.dumps(pin(arg), indent=2))
