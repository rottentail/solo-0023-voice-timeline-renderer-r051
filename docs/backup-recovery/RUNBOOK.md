# 在线备份与恢复手册（Voice Timeline Renderer）

本手册基于仓库当前**已存在**的工具与机制，描述如何在服务持续接收素材与渲染的前提下做在线备份、如何判定一份副本能否用于恢复、如何在不触碰现有合法数据的前提下完成隔离恢复与切换。手册中的每一条承诺都由可重放的隔离演练证据支撑（见第 9 节与 `docs/backup-recovery/evidence/`）。

> 范围声明：本手册不依赖、也不引用任何尚未实现的"快照接口"或未来 API。服务当前没有冻结/快照端点，本手册不假设它存在，也不把"建议停机"作为默认方案。生产代码在本轮没有任何行为修改；新增内容仅为运维工具、脚本、文档与隔离演练。

## 1. 为什么"文件齐全"不等于"可恢复"

服务把四类数据放在同一个数据目录（`README.md` 的"Data directory"）：

```text
<data>/
  sources/<source_id>/{source.wav, metadata.json}
  timelines/<timeline_id>.json
  jobs/<render_id>.json
  renders/<render_id>/{output.wav, receipt.json}
  tmp/
```

持久化机制（`internal/storage/`）决定了在线 `cp -a` 能观察到的中间态：

- 单条 metadata（source、timeline、job 的每次状态流转）用"同目录临时文件 + `rename`"原子替换；
- source 目录与 render 目录先在 `tmp/` 组装，再用一次目录 `rename` 发布，发布后内容不可变；
- **但这些原子性只覆盖单个文件/单个目录，跨文件之间没有事务**。

因此一次与业务并发的目录复制，可能落在这些窗口里：

1. `jobs/<id>.json` 已复制到 `rendering/finalizing`，而 `renders/<id>/` 的原子 rename 还没发生 → 副本里有非终态作业、没有产物；
2. 复制进行中某个文件只写了一半（JSON 截断、WAV 截断）；
3. render 目录被复制了，但对应的 job 记录尚未/未被复制 → 孤儿产物；
4. 复制窗口内新提交的 source/timeline/render 部分落盘、部分未落盘，产生跨文件引用缺口。

"每个预期路径都有文件"无法排除以上任何一种情况。可恢复性必须由**跨文件一致性核验**来判定，而不是文件清单。

## 2. 工具与产物（本轮新增，均为现有代码可构建）

| 组件 | 位置 | 作用 |
|---|---|---|
| 核验/恢复库 | `internal/backupverify/` | 唯一的核验规则实现，CLI 与演练共用，避免规则两份漂移 |
| 核验/恢复 CLI | `cmd/backupverify/` | `verify` / `manifest` / `check-manifest` / `restore` |
| 隔离演练 | `cmd/backupdrill/` | 用真实 HTTP 服务在临时目录里重放四类场景，产出文本证据 |
| 在线备份脚本 | `ops/backup-recovery/online-backup.sh` | 不停机复制 + 多轮收敛 + 仅晋升通过核验的副本 |
| 隔离恢复脚本 | `ops/backup-recovery/isolated-restore.sh` | 空目录恢复 + 防覆盖保护 + 恢复后回放核验 |
| 演练入口 | `ops/backup-recovery/run-drill.sh` | 构建并运行演练、刷新证据 |

构建（不依赖 cgo，`bin/` 已在 `.gitignore` 中）：

```bash
CGO_ENABLED=0 go build -buildvcs=false -o bin/backupverify ./cmd/backupverify
CGO_ENABLED=0 go build -buildvcs=false -o bin/backupdrill  ./cmd/backupdrill
CGO_ENABLED=0 go build -buildvcs=false -o bin/voiceservice ./cmd/voiceservice
```

### 2.1 `backupverify verify` 的判定规则

`verify` 只读、绝不修改被检目录。它检查并交叉核对：

- 顶层五个目录是否齐全、是否有未声明条目；
- 每个 source：`metadata.json` + `source.wav` 是否齐全，metadata 的 `id`、`digest`、`byte_count` 是否与实际字节一致；
- 每个 timeline：是否为已提交的 `ready`，每个 span 引用的 source 是否都在副本内；
- 每个 job：
  - `queued/rendering/finalizing` → 计入 **in-flight**，并断言其**没有** render 目录；
  - `completed` → 必须有 output 元数据、receipt digest、完整的 `renders/<id>/`，且 output 实际 SHA-256 同时等于 receipt、job 两处记录，receipt 中每个 source digest 与现存 source 一致，引用的 timeline 存在；
  - `failed` → 必须带稳定错误码、不得带 output、不得有 render 目录；
- 每个 render 目录：必须能对应到一个 `completed` job，否则判为孤儿；
- 原子写临时文件（`.write-*`）与 `tmp/` 残留只记为 info，不阻断（这是在线复制的正常现象）。

输出 `status`：

- `valid`：没有 blocker。副本中所有**已闭合**的工程与交付自洽、可恢复；in-flight 作业单独列名，不被算作已完成。
- `invalid`：至少一个 blocker。该副本**不得**用于恢复或切换。

退出码：`0` valid，`2` 存在 blocker，`1` 用法/运行时错误。`-json` 产出完整结构化报告。

## 3. 在线备份（服务全程不停机）

### 3.1 标准流程

```bash
./ops/backup-recovery/online-backup.sh \
  -d /var/lib/voice-timeline \
  -b /var/backups/voice-timeline \
  -r 3 -s 2
```

脚本对每一轮：

1. 在服务**保持运行、持续可写**的情况下 `cp -a` 数据目录到一个全新的 round 目录；
2. 立即对该 round 跑 `backupverify verify`；
3. 仅当 `status=valid` 才把它原子地晋升为 `backups/current`（符号链接切换），并在 round 目录之外写 manifest 与核验报告（不污染数据布局）；
4. 若该轮 `invalid`（撞上撕裂窗口），保留现场用于排查，睡眠后用一个新 round 重试，**绝不用坏副本覆盖上一个 good 副本**；
5. 所有轮次都失败则以退出码 2 失败，并显式保留既有 `current/` 不动。

这是在线复制撕裂窗口的标准收敛手段：复制本身无事务点，但重轮 + 晋升闸保证被命名为"当前备份"的副本一定通过了跨文件一致性核验。

### 3.2 恢复点边界（必须如实承诺）

- 备份能承诺的，仅限核验报告 `closed_history` 中的 sources / ready timelines / `completed` renders / `failed` renders；
- `in_flight_jobs` 列表中的作业是复制窗口里**正在推进**的任务，它们在副本中不是 completed，**不得在任何交付说明中写成已完成**；
- 复制窗口内、落盘晚于复制的新数据可能完全不在副本中（演练 `live-write` 实测：9 个 live 完成渲染中副本捕获 3 个，其余明确列入 missing，而不是被假装存在）。

### 3.3 远端/对象存储

把已晋升的 round 目录（不含报告/manifest 的纯数据副本）上传到远端时，上传后必须在远端或重新拉回后再跑一次 `verify`，并可用 `manifest` + `check-manifest` 做字节级漂移检测。远端不可信，上传成功不代表内容自洽。

## 4. 恢复流程（隔离、防覆盖、只承诺已保存的历史）

### 4.1 硬性保护

`backupverify restore` 与 `isolated-restore.sh` 都强制：

- 恢复目标**不存在或为空目录**；目标非空立即拒绝，不做任何合并、覆盖或"先备份再覆盖"；
- 恢复前先对来源副本 `verify`，`invalid` 直接中止（退出码 2）；
- 恢复完成后对目标再跑一次 `verify`，并起一个隔离端口的真实服务做回放。

因此恢复演练永远不会覆盖现有生产或其它合法数据（由 `restore-guard` 演练证明：非空目标被拒绝、标记文件内容保持不变）。

### 4.2 标准恢复与回放

```bash
./ops/backup-recovery/isolated-restore.sh \
  -b /var/backups/voice-timeline/current \
  -t /var/lib/voice-timeline.restored \
  -p 18099
```

脚本会：核验来源 → 复制到空目标 → 再核验 → 起隔离服务 → 对每个 `completed` job 经真实 HTTP 拉 `output` 并校验其 SHA-256 等于 job/receipt 记录；非终态作业只提示、不回放。

### 4.3 进行中作业的处置（不能假装完成）

实测确认（见 `drill-live-write.txt`）：恢复出的 `rendering/queued/finalizing` 作业，服务**不会自动续跑**；对同一 `render_id` 再次 `PUT /v1/renders/{id}` 因幂等逻辑只返回既有作业（HTTP 200），**不会重新执行**。处置规则：

1. 以核验报告的 `in_flight_jobs` 为准，向调用方声明这些渲染未完成、无交付物；
2. 其 timeline 与 source 若在闭合历史中仍存在，则用**新的 render ID** 重新 `PUT /v1/renders/{new_id}` 发起渲染；
3. source 重传是幂等的（同 ID + 同字节返回既有记录），可安全重试；
4. 绝不把 in-flight 作业手工改成 `completed`，也不凭 `renders/` 里的残留目录宣称交付——核验器会把这种状态判为 blocker。

### 4.4 恢复实例能承诺什么

只承诺它确实保存且通过核验的内容：

- 可下载且 output 字节 digest 与 receipt/job 三方一致的 completed 渲染；
- digest 自洽的 source 与引用闭合的 ready timeline；
- 带稳定错误码的 failed 记录（如实保留为失败，不吞掉）。

副本里没有的工程、复制窗口之后才产生的素材与渲染，一律不在承诺范围内。

## 5. 切换依据（cutover criteria）

只有同时满足下列条件，才允许把流量切到恢复实例：

1. 来源备份 `verify` 与目标目录恢复后 `verify` 均为 `status=valid`（退出码 0、无 blocker）；
2. 隔离服务 `GET /healthz` 返回正常；
3. 每个 `completed` render 经 HTTP 回放，output SHA-256 与 job 的 `receipt_digest`、receipt 的 `output.digest` 全部一致（脚本自动执行并逐条打印 `replay OK`）；
4. in-flight 清单已交付给业务方，且重提策略（新 render ID）已就绪；
5. 恢复实例监听独立地址，切换是流量层面的决定，原数据目录在切换确认前保持只读保留、不删除。

### 无法兑现时的处置

- 备份 `invalid` 或回放 digest 不一致：**不切换**。回到第 3 节重新取在线备份（服务仍在运行，可持续产生新副本）；若所有可用副本都失败，用保留的上一个 `current/`（晋升机制保证它曾通过核验），并按其核验报告如实缩小承诺范围。
- 目标非空导致恢复被拒：更换为空目标，禁止手工清空疑似生产目录。
- 恢复实例缺业务所需的最新工程：明确告知数据缺口区间（以备份报告时间与 in-flight/missing 清单为证据），由调用方在恢复实例上用新 ID 重新提交，而不是回写伪造状态。
- 任何一步失败都保留现场与 JSON 报告，禁止"先切过去再说"。

## 6. 不做什么（明确的取舍）

- **不**把"建议停服做冷备"作为方案：本流程用重轮收敛 + 核验闸实现在线一致性，停服仅是用户在极端情况下可自行选择的手段，不是本手册的前置条件。
- **不**依赖未实现的快照/冻结 API；服务没有该端点，所有判断只基于文件契约与 HTTP 只读接口。
- **不**以文件数量/路径齐全作为可恢复判据。
- **不**在非空目录上恢复，**不**手工改 job 状态，**不**把 in-flight 记为 completed。
- **不**让核验/恢复工具写入被检的 live 数据目录（工具对来源只读；写操作只发生在新建的备份/恢复目录）。

## 7. 常规运营节奏建议

- 备份频率按 RPO 设定（例如每 15 分钟一轮），每轮都必须过核验闸；保留最近 N 个 round 与全部晋升报告。
- 每次服务版本升级后、至少每月，跑一次 `run-drill.sh` 刷新证据，确认存储契约未漂移。
- 值班交接以最近一次 `verify` JSON 报告中的 `closed_history` 计数与 `in_flight_jobs` 作为恢复点事实来源。

## 8. 命令速查

```bash
# 构建
CGO_ENABLED=0 go build -buildvcs=false -o bin/... ./cmd/...

# 在线备份（不停机，自动多轮收敛）
./ops/backup-recovery/online-backup.sh -d <live-data> -b <backup-root>

# 单独核验一份副本
./bin/backupverify verify -root <copy> -json <report.json>

# 字节级清单与漂移检测
./bin/backupverify manifest -root <copy> -out <copy.manifest.json>
./bin/backupverify check-manifest -root <copy> -manifest <copy.manifest.json>

# 隔离恢复（目标必须为空）
./ops/backup-recovery/isolated-restore.sh -b <backup> -t <empty-target> -p 18099

# 重放全部隔离演练并刷新证据
./ops/backup-recovery/run-drill.sh            # 全部场景
./ops/backup-recovery/run-drill.sh -s live-write
```

## 9. 隔离演练与可重放证据

`backupdrill` 每次运行都在 `os.MkdirTemp` 生成的独立工作区里启动**真实的生产 HTTP handler**（`httpapi.New` + 真实 `storage.Repository`），不接受任何外部数据路径，因此演练物理上无法触碰生产数据。四个场景：

| 场景 | 证明的命题 | 证据文件 |
|---|---|---|
| `closed-history` | 完整闭合副本核验 valid；恢复实例逐 render 回放，output digest 与备份完全一致 | `drill-closed-history.txt` |
| `torn-copies` | 6 类不完整/损坏副本全部被 blocker 拦截（缺 render 目录、缺 output.wav、output 被篡改、source digest 不符、孤儿 render 目录），而"作业进行中无产物"被正确标为 in-flight 而非 completed | `drill-torn-copies.txt` |
| `live-write` | 复制窗口内服务持续接受写入、零停机；副本只承诺实际捕获的闭合历史；确定性撕裂作业在恢复实例上不被暴露为 completed；原 live 目录事后核验仍 valid、未被改动 | `drill-live-write.txt` |
| `restore-guard` | 恢复到非空目标被拒绝且原文件字节不变；空目标恢复干净并再核验通过 | `drill-restore-guard.txt` |

重放方式（随时可重新生成证据）：

```bash
./ops/backup-recovery/run-drill.sh
# 证据输出到 docs/backup-recovery/evidence/drill-*.txt
```

这些证据文件是演练的真实 stdout 存档；其断言由 `internal/backupverify` 的同一套规则产生，与生产恢复路径使用的是同一份核验代码，而不是两套逻辑。
