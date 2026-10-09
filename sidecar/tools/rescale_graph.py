"""Re-bake the isnet-anime ONNX graph for another square input size.

ISNet is fully convolutional, but the pytorch-1.10 export hard-codes the
Resize targets as INT64 `sizes` initializers ([1, C, H, W]) and the IO dims as
1024. This rewrites the input/output dims and scales the H/W of every Resize
`sizes` by NEW/1024, so the same weights run at e.g. 512x512.

Usage: python -I rescale_graph.py SRC.onnx DST.onnx NEW_SIZE
"""
from __future__ import annotations

import sys

import numpy as np
import onnx
from onnx import TensorProto, numpy_helper


def rescale(src: str, dst: str, new: int) -> None:
    m = onnx.load(src)
    g = m.graph
    inp, out = g.input[0], g.output[0]
    old = inp.type.tensor_type.shape.dim[2].dim_value
    assert old == inp.type.tensor_type.shape.dim[3].dim_value, "square input expected"
    f = new / old
    for vi in (inp, out):
        for d in vi.type.tensor_type.shape.dim[2:]:
            d.dim_value = new

    sizes_names = set()
    for n in g.node:
        if n.op_type == "Resize" and len(n.input) >= 4 and n.input[3]:
            sizes_names.add(n.input[3])
    n_done = 0
    for t in g.initializer:
        if t.name in sizes_names:
            assert t.data_type == TensorProto.INT64, t.name
            arr = numpy_helper.to_array(t).copy()
            assert arr.shape == (4,), (t.name, arr)
            for k in (2, 3):
                v = arr[k] * f
                assert abs(v - round(v)) < 1e-6, f"{t.name}: {arr} * {f} is not integral"
                arr[k] = int(round(v))
            t.CopyFrom(numpy_helper.from_array(arr, t.name))
            n_done += 1
    del g.value_info[:]  # stale shapes; ORT re-infers
    onnx.checker.check_model(m)
    onnx.save(m, dst)
    print(f"rescaled {n_done} Resize sizes {old} -> {new}: {dst}")


if __name__ == "__main__":
    rescale(sys.argv[1], sys.argv[2], int(sys.argv[3]))
