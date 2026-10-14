# 原始迁移证据

本目录 JSON 记录 2026-10-07 迁移时的文件清单、哈希、数据库路径修复及推理验证；本轮保持原样，不作为当前配置或验收结论。

原始 Markdown 报告见 [归档](../archive/2026-10-12/docs/migration/Migration-report.md)。当前系统见 [状态说明](../CURRENT_STATUS.md)。

| 文件 | 历史用途 |
|---|---|
| `source-copy-manifest.json` | 搬迁文件、大小与哈希，包含当时存在而如今已删除的文件 |
| `database-migration.json` | 工作数据库路径修复、历史缺失结果记录 |
| `validation-database-migration.json` | 验证数据库路径修复 |
| `state-verification.json` | 搬迁状态核对 |
| `onnx-cpu-smoke.json` | 当时模型 CPU 试运行 |
| `detection-smoke.json` | 当时检测管线试运行 |

报告中的缺失结果指向原始数据缺口，本轮不恢复、不重算，也不改动数据库。
