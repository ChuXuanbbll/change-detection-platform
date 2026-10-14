import numpy as np
import onnxruntime as ort

# =========================
# 1. 模型路径
# =========================
model_path = r"D:\2026级-xxx\变化检测平台\模型部署\model\enforcement_2.onnx"

# =========================
# 2. 加载 CUDA DLL
# =========================
ort.preload_dlls(directory="")

# =========================
# 3. 创建 GPU 推理会话
# =========================
session = ort.InferenceSession(
    model_path,
    providers=[
        "CUDAExecutionProvider",
        "CPUExecutionProvider"
    ]
)

print("=" * 60)
print("模型加载成功")
print("实际 Providers:", session.get_providers())

# =========================
# 4. 查看输入
# =========================
print("\n输入信息：")
for x in session.get_inputs():
    print("名称:", x.name)
    print("Shape:", x.shape)
    print("类型:", x.type)

# =========================
# 5. 创建两张测试影像
# =========================
image_a = np.random.rand(
    1, 3, 512, 512
).astype(np.float32)

image_b = np.random.rand(
    1, 3, 512, 512
).astype(np.float32)

print("\n测试输入：")
print("image_a:", image_a.shape, image_a.dtype)
print("image_b:", image_b.shape, image_b.dtype)

# =========================
# 6. 执行模型推理
# =========================
outputs = session.run(
    None,
    {
        "image_a": image_a,
        "image_b": image_b
    }
)

# =========================
# 7. 查看输出
# =========================
print("\n推理成功！")
print("输出数量:", len(outputs))

for i, output in enumerate(outputs):
    print(f"\n输出 {i}:")
    print("Shape:", output.shape)
    print("dtype:", output.dtype)
    print("最小值:", output.min())
    print("最大值:", output.max())
    print("均值:", output.mean())

print("\n" + "=" * 60)
print("ONNX GPU 推理测试完成")