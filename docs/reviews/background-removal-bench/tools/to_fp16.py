"""Make an fp16 copy of the isnet-anime ONNX graph (opset 11) without onnxconverter-common.

* every FLOAT initializer -> FLOAT16, except the `roi` / `scales` inputs of
  Resize nodes (the opset-11 op requires float32 there)
* graph input `img` stays float32: a Cast(to=FLOAT16) node feeds the network
* the network's float16 `mask` goes through Cast(to=FLOAT) so the output stays
  float32 [1,1,H,W]
* every internal value_info FLOAT -> FLOAT16 (shape inference re-derives the rest)

Usage: python -I to_fp16.py SRC.onnx DST.onnx
"""
from __future__ import annotations

import sys

import numpy as np
import onnx
from onnx import TensorProto, helper, numpy_helper


def convert(src: str, dst: str) -> None:
    m = onnx.load(src)
    g = m.graph
    keep_fp32 = set()
    for n in g.node:
        if n.op_type == "Resize":
            for idx in (1, 2):  # roi, scales
                if idx < len(n.input) and n.input[idx]:
                    keep_fp32.add(n.input[idx])

    n_conv = 0
    for t in g.initializer:
        if t.data_type == TensorProto.FLOAT and t.name not in keep_fp32:
            arr = numpy_helper.to_array(t).astype(np.float16)
            t.CopyFrom(numpy_helper.from_array(arr, t.name))
            n_conv += 1

    for vi in g.value_info:
        if vi.type.tensor_type.elem_type == TensorProto.FLOAT:
            vi.type.tensor_type.elem_type = TensorProto.FLOAT16

    assert len(g.input) == 1 and len(g.output) == 1
    in_name, out_name = g.input[0].name, g.output[0].name
    in16, out16 = in_name + "_fp16", out_name + "_fp16"
    for n in g.node:
        for i, name in enumerate(n.input):
            if name == in_name:
                n.input[i] = in16
        for i, name in enumerate(n.output):
            if name == out_name:
                n.output[i] = out16
    g.node.insert(0, helper.make_node("Cast", [in_name], [in16], name="cast_input_fp16", to=TensorProto.FLOAT16))
    g.node.append(helper.make_node("Cast", [out16], [out_name], name="cast_output_fp32", to=TensorProto.FLOAT))

    onnx.checker.check_model(m)
    onnx.save(m, dst)
    print(f"converted {n_conv} initializers to fp16 (kept fp32: {sorted(keep_fp32)[:4]}{'...' if len(keep_fp32) > 4 else ''}, n={len(keep_fp32)}) -> {dst}")


if __name__ == "__main__":
    convert(sys.argv[1], sys.argv[2])
