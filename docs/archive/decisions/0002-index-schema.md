# 历史存档 0002：记忆索引表设计（index.db）

> 已被 memgov v2 取代。下文的文件真相源、atoms 表及重建方案仅是旧版记录；0003 也已归档。当前 SQLite `state.db` 保存权威数据，只有检索索引可重建；见[现行架构](../../architecture/architecture.md)和[CLI 用法](../../../README.md)。

- 状态：**历史存档，不再适用**（旧版表结构曾由 0003 接替）
- 日期：2026-09-11
- 承接：`0001-memory-storage.md`（记忆本体 = 文件，`index.db` = 派生索引）
- 可执行 DDL：[`0002-index-schema.sql`](0002-index-schema.sql)（sqlite 3.51.0 / FTS5 trigram 验证通过）

## 结论

**index.db 只做一件事：把「全扫 + 全量 Parse」换成一次查询。它不持有任何独家信息。**

五张表，全部可从 `.memory/*.md` 全量重建：

| 表 | 装什么 | 丢了会怎样 |
|---|---|---|
| `atoms` | 原子全部 frontmatter 字段 + body + sha256 | 重建 |
| `atom_about` | 指向轴（轴三）的多值展开 | 重建 |
| `atom_edges` | 溯源边（related / result_of / …） | 重建 |
| `atoms_fts` | description + body 的 trigram 全文索引 | 重建 |
| `meta` | schema 版本 / 数据根 / 重建时间 | 重建 |

所以 schema 变动的处理方式不是迁移，是**删库重建**（见第 3 节第 6 条）。

## 1. 边界（先写死，防止跑偏）

| 数据 | 进 index.db | 理由 |
|---|---|---|
| 记忆原子 | ✓ | 就是它的存在意义 |
| 指向轴 `about` / 溯源边 | ✓ | 召回与图谱的查询维度 |
| 治理审计线 `.governance.jsonl` | **✗** | **它是 append-only 真相，不是派生** |
| 执行轨迹 `traces/` `artifacts/` | **✗** | 属 `internal/store` 域，与记忆索引无关 |
| 墓碑 `.tombstones.jsonl` | **✗** | 同上，是防蒸馏重生的真相 |
| 向量 / embedding | **✗** | 会让库变成第二真相，且当前不需要 |

**最容易搞错的一条**：审计线不进库。ADR-033 要求治理动作可回放，`.governance.jsonl` 就是那份证据。
把它做成派生表，等于把证据降级成缓存——库一重建，`memory.changes` 的历史就没了。

## 2. 字段映射

`atoms` 主表与 `memory.Atom` 一一对应，**包括归一的证据链**：

| Atom 字段 | atoms 列 | 说明 |
|---|---|---|
| `Slug` | `slug` | 保留 `atom-` 前缀（`CanonicalID` 形态） |
| `Type` / `TypeRaw` / `TypeIssue` | `type` / `type_raw` / `type_issue` | 三件套：归一值 + 原始值 + 原因 |
| `Granularity` | `granularity` / `granularity_raw` / `granularity_issue` | ⚠️ 见第 7 节，后两列**依赖先修代码** |
| `State` | `state` / `state_raw` / `state_issue` | ⚠️ 同上 |
| `About` | → `atom_about` | 拆表，保留顺序与原始字面值 |
| `Related` / `ResultOf` | → `atom_edges` | 拆表，`kind` 区分 |
| `FromTurn` / `FromFile` | `from_turn` / `from_file` | 独立列，**不进边表** |
| `Body` | `body` | 同时喂 `atoms_fts` |
| `Path` / `SHA256` | `git_path` / `sha256` | 治理并发闸用 |
| `Issues` / `Extra` | `issues` / `extra`（JSON） | 证据，不是查询维度，不拆表 |
| `Raw` | — | 不入库，是解析中间态 |

**`*_raw` 不落盘**：文件里永远只有规范值，raw 是「这次解析看到了什么」的现场证据，
每次重建都重新解析得到，所以进索引不构成独家信息。这与「写操作会把文件归一化」不冲突——
`Render` 只在 `Write` 路径调用，只读的 `List` / `Validate` 不动文件。

**`workspace` 与 `path`**：`workspace` 是列不是分库（0001 决策：一个 `index.db`，workspace 用列区分）。

## 3. 刻意的设计选择

1. **`id INTEGER PRIMARY KEY` + `UNIQUE(workspace, slug)`**，而不是 slug 直接当主键。
   整数主键让 external-content FTS5 的 `content_rowid` 稳定，也让边表引用更省空间。
   slug 查询走 UNIQUE 索引，一样快。

2. **`about` 拆表，但保留 `ord` 和 `raw`**。
   `ord` 是硬要求：`Render` 必须是不动点，`about` 顺序影响渲染，索引里丢了顺序，
   重建就可能写出不同 diff——那正是「每次写入都产生无意义 diff」的病根。
   `raw` 是纪律要求：归一不静默，原始字面值本身是证据。

3. **边表的目标用 `dst_slug`，且刻意不加外键**。
   悬空边是**合法状态**：`result_of` 可能指向尚未写出的原子。强行加外键会导致两种坏结果——
   插入失败，或被迫为不存在的目标建占位行（污染图谱）。
   悬空边交给 `memory.validate` 报 issue，而不是让外键拒绝写入。
   来源侧（`src_id`）则必须级联，删原子就删它的边。

4. **`from_turn` / `from_file` 不进边表**。
   它们指向 trace 和外部文件，不是原子。放进 `atom_edges` 会让「边」这个概念的语义开裂。

5. **FTS 用 `trigram`，且连 `description` 一起索引**。
   trigram 是中文的必需项——默认 `unicode61` 对中文按整串切词，基本不可用（0001 实测）。
   description 一并进：标题命中通常比正文命中更准。
   用 external content 模式，不存第二份正文。

6. **派生层不做 migration**。
   `meta.schema_version` 不匹配时，正确处理是**全量重建**，不是写迁移脚本。
   库随时可弃，为它维护迁移历史是纯粹的负债。

7. **索引列序按「等值条件的固定性」排**：`(workspace, state, namespace, type)`。
   `workspace` 必然等值 → `state` 召回固定 `active` → `namespace` 是 IN 列表 → `type` 是可选过滤。
   （此处与 0001 的 schema 草稿 `(namespace, type, state)` 不同，以本设计为准。）

## 4. 不变量（进代码注释与测试）

- **写路径先落文件、后刷索引**；索引刷新失败**不回滚文件**（索引可重建）。
- **需要精确字节的只读文件**：`read_atom`、`Write` 前的 sha256 闸。
  索引只服务 `search` / `list` / `recall` / 聚合。
- **`rm index.db` + `memgov index rebuild` 的结果，必须与增量维护的结果逐行一致**。
  这是唯一能证明「索引是派生」的测试，必须有。
- **索引不进 git**（`.gitignore`）。真相永远是文件。
- **每个连接必须开 `PRAGMA foreign_keys`**（见第 5 节，Go 驱动用 DSN 参数）。

## 5. 已验证行为 + 一个静默坑

在 sqlite 3.51.0（FTS5 + trigram）上跑通 8 项：召回热路径、about 反查、悬空边、
中文 FTS、UPDATE 后 FTS 无残留旧词、DELETE 级联、幂等 upsert、索引清单。

**坑（实测对照）**：

| | atoms 残留 | about 残留 | edges 残留 | FTS 残留 |
|---|---|---|---|---|
| 开 `PRAGMA foreign_keys=ON` | 0 | **0** | **0** | 0 |
| 不开（驱动忘配 DSN） | 0 | **1** | **1** | 0 |

`PRAGMA foreign_keys` **默认 OFF，且是连接级设置**。写在建表脚本里无效——
必须每个连接打开。否则 `ON DELETE CASCADE` 静默失效：

- 召回看起来完全正常（只 join `atoms`）
- 但 `about` / 边反查会返回**已删除原子的幽灵行**
- 且不会有任何报错

Go 侧落点：驱动 DSN 加 `_pragma=foreign_keys(1)`，并在重建后加一条断言测试。

## 6. 落地顺序（风险最低的路径）

1. **只做 `index rebuild`**：全量扫 `.memory/*.md` → 建库。**不接写路径**。
   此时索引是纯只读快照，错了重跑就是，风险为零。
2. 把 `List()` / `Active()` 的读路径切到索引，对照测试「索引结果 ≡ 文件全扫结果」。
3. 最后才接增量：`Write` / `Retire` / `Restore` 成功后 upsert 索引。

**不要一步到位**。第 1 步单独就有价值——它把「重建结果与增量一致」这个不变量先钉死。

## 7. 前置修复（代码侧，必须先做）

设计表时核对 `memory.Atom`，发现**三条轴不对称**：

| 轴 | 归一值 | 原始值 `*_raw` | 原因 `*_issue` |
|---|---|---|---|
| `type` | ✓ | ✓ `TypeRaw` | ✓ `TypeIssue` |
| `granularity` | ✓ | **✗ 没有** | **✗ 没有**（`granIssue` 只进 `Issues`，原始值丢弃） |
| `state` | ✓ | **✗ 没有** | **✗ 没有**（`stateIssue` 只进 `Issues`，原始值丢弃） |

后果：文件里写 `granularity: sop`，归一成 `atom` 后，**原始值 "sop" 永久消失**——
连 `memory.validate` 都看不到模型写错过什么。这违反硬约定
「归一出词表的值不静默：夹回兜底值 + 原始值留 `*_raw` + 原因留 `issues`」。
`type` 做到了，另两轴没有。

**修复**：给 `Atom` 补 `GranularityRaw` / `GranularityIssue` / `StateRaw` / `StateIssue`，
`Parse` 里按 `TypeRaw` 的同款写法填。`*_raw` **不落盘**（不进 `KnownFieldOrder`），
只作内存证据——文件里仍是规范值。

表结构里这两组列已预留，代码补上即可启用；若暂不补，则这几列恒为空串。

## 8. 触发信号（什么时候真写代码）

- 本机实测：`List()` 在 **1,000 条 = 314ms**、1 万条 = 1.7s、5 万条 = 13.4s。
  瓶颈是 `Parse()` 的 yaml 解析（约 268µs/文件），不是 IO。
- 当前真实规模：**2 条原子**。
- **触发线：原子数到 1,000 ~ 2,000，或 `memory.atom.list` 频繁超过 300ms。**

但注意：`List()` 目前只在 CLI 手敲时触发，不在每轮模型上下文里。
**真正的引爆点是「召回管线」**——一旦每轮对话都要扫一遍记忆，门槛会立刻压到几百条。
所以第 6 节第 1 步（只做 rebuild）可以在召回管线动工时同步做，不必等到 1,000 条。

## 附：不做的事

- 不把记忆本体搬进库（ADR-002 已否过两次，理由见 0001 第六节）
- 不做 schema migration（重建即可）
- 不建向量索引
- 不把审计线 / trace / 墓碑放进库
- 不做图谱的独立边表之外的图算法（BFS、路径查询留到真需要时）
