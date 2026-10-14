"""Run in the configured inference interpreter, not the web service environment."""

import importlib.util
import json
import sys
from types import SimpleNamespace
import numpy as np

if __name__ == '__main__':
    spec = importlib.util.spec_from_file_location('pipeline', sys.argv[1])
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    mapping = json.loads(sys.argv[3])
    args = SimpleNamespace(
        onnx_model=sys.argv[2],
        provider='cuda',
        device='cuda:' + sys.argv[4],
        ort_intra_op_threads=0,
        worker=False,
        input_a_name=mapping.get('input_a'),
        input_b_name=mapping.get('input_b'),
        output_name=mapping.get('output'),
        debug_stats=False,
        tile_size=512,
    )
    detector = module.ONNXChangeDetector(args)
    result = detector.infer([np.zeros((6, 512, 512), dtype=np.float32)])
    if (
        result.shape[1] not in (1, 2)
        or result.shape[-2:] != (512, 512)
        or not np.isfinite(result).all()
    ):
        raise ValueError('需要有限的 NCHW logits，1或2个输出通道，512×512 输出网格')
    print(
        json.dumps(
            {'providers': detector.session.get_providers(), 'output_shape': list(result.shape)}
        )
    )
