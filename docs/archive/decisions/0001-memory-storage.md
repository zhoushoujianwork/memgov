# 历史存档 0001：记忆存储 —— 文件原生，还是直接上数据库？

> 已被 memgov v2 取代。下文保留当时分析，不是现行操作指南。当前以 SQLite `state.db` 为唯一真相源，使用 Source、Candidate、Review、Memory；见[现行架构](../../architecture/architecture.md)和[CLI 用法](../../../README.md)。

- 状态：**历史存档，不再适用**（原状态：待拍板）
- 日期：2026-09-11
- 关联：`relayer-old` 的 `docs/adr-002` 决策 4 / `docs/adr-023` 决策 7、8 / `docs/adr-004` 决策 1、8 / `docs/design-conventions.md` 不变量 1 / `docs/adr-025` 检索成本实测

## 结论（先给答案）

**不要二选一。**

- **记忆本体 = 文件**（`<workspace>/.memory/atom-*.md` + git）= 唯一真相。
- **SQLite（`index.db`）= 派生索引**，可从文件全量重建，丢了不心疼。

这不是新方案 —— 正是 ADR-002 决策 4 早就写死、却一直空着的那个 `index.db` 位置
（ADR-023 决策 7 原话：「兑现一个从没建的设计」）。

「记忆直接全放数据库」这条**在 ADR-002 已被否过两次**（ADR-001 修订 + agent_room 2026-07-07 定案），
否决理由**不是性能**，是下面第二节那几条。

## 一、先把三类数据分开（「散落」其实有三层，别混谈）

| 类别 | 落在哪 | 是记忆吗 | 现状 |
|---|---|---|---|
| 原子 / 墓碑 / 纪事 / 摘要 | `<ws>/.memory/*.md` 等**文件** | 是 | 文件原生，进 git |
| 调度运行态 `tasks/runs/lease` | `~/.relayer/kernel/kernel.db` | 否（运行日志） | **本来就在 SQLite** |
| 派生索引（画像 / 图谱 / 召回） | `~/.relayer/index.db` | 否（可弃） | 位置留了，**还没建** |

**「用数据库」在 relayer 里不是禁忌**：`kernel.db` 已经在用 `modernc.org/sqlite`（纯 Go，`CGO_ENABLED=0` 也能交叉编译）。
所以加 `index.db` **不引入任何新的依赖类别**。

真正被否的只有一件事：**让数据库当记忆的真相来源**。

## 二、逐项对比

| 维度 | 文件原生（建议保持） | 纯数据库当真相 | 说明 |
|---|---|---|---|
| 谁读写 | Claude / agent 天然读写文件与 `CLAUDE.md` | 每轮蒸馏要额外写 DB 代码 | 直接决定蒸馏管线形态；这是 ADR-002 否掉纯 DB 的头号理由 |
| 人可读 / 可改 | 直接编辑 `.md` | 需要工具或客户端 | 机主能手工纠错 |
| 演进审计 | `git log` = 记忆演化史，`git diff` = 每轮改了什么 | 得自己造审计表 | ADR-033 的治理审计线依赖「可回放」 |
| 回滚 / 离线 | git 天然支持 | 复杂 | |
| schema 迁移 | **没有 schema 就没有迁移** | 每次改字段都要迁移 | design-conventions 明确点过 |
| 真相份数 | 只有一份，不会漂 | 纯 DB 则绑定 schema；文件+DB 双写才会漂 | |
| 结构化查询 | O(N) 全扫（实测见第三节） | 索引，快 | **DB 的真实优势** |
| 全文检索 | `grep` / `ripgrep`，无排序加权 | FTS5 | DB 优势 |
| 并发写 | 需自己加锁 | 事务 | 单机单用户，影响小 |
| 损坏风险 | 纯文本，坏了可读可修 | 二进制损坏可能全丢 | |
| 跨 workspace 聚合 | 遍历目录（ADR-025 实测 **10.24ms / 12.85ms，占 80%**） | 一条 SQL | DB 优势，正是 `index.db` 的用途 |

## 三、实测：性能到底有没有问题

方法：本机生成 N 个符合 ADR-004 schema 的原子文件（frontmatter 13 键 + 一句正文），对比

- (a) **文件全扫**：`readdir` + 逐文件读 + 解析 + 过滤
- (b) **`MEMORY.md` 单文件**：一行一条的合并索引，只读 1 个文件
- (c) **`index.db`**：`atoms` 表 + B-tree + `FTS5(trigram)`

每项 20 轮取平均、预热后计（稳态）。

| 原子数 | 文件全扫 / 次 | MEMORY.md 单文件 | 索引·结构化召回 | 索引·全文 FTS | 建索引（一次） | 索引体积 |
|---|---|---|---|---|---|---|
| 2 千 | 200 ms | 0.5 ms | 0.06 ms | 2.4 ms | 0.22 s | 1.1 MB |
| 1 万 | 886 ms | 2.0 ms | 0.05 ms | 10 ms | 2.4 s | 5.4 MB |
| 5 万 | **7.93 s** | 11 ms | 0.49 ms | 65 ms | 10 s | 27 MB |

（对照：文件文本体积 2 千=0.6 MB / 1 万=2.9 MB / 5 万=14.8 MB）

读数：

1. **文件全扫是 O(N)，而且 syscall-bound**（每文件一次 open+read ≈ 90–160 µs）。1 万就已接近 1 秒；**若每轮召回都扫，直接吃满交互预算**。
2. **索引结构化召回基本与 N 无关**（0.05–0.5 ms）。这是 ADR-004 的热路径（`namespace` + `type` + `state`）。5 万时比全扫快约 **1.6 万倍**。
3. **FTS 全文在中文上不便宜**（trigram，10–65 ms）。建议「先用 B-tree 收敛候选，再 FTS」，**别拿 FTS 当召回主路径**。
4. **`MEMORY.md` 单文件是文件原生的合法优化**（N 次读 → 1 次读），5 万时 11 ms，比全扫快约 700 倍；但它是线性文本扫描，**不能排序 / 多条件 / 聚合**。
5. 建索引是**一次性**成本且**可随时重建**；索引体积 ≈ 文本的 1.8 倍，很便宜。

> 注意：基准跑在沙箱临时目录，文件系统偏慢（~90–160 µs/文件）。真机 NVMe 可能快 2–4 倍，
> 但**「线性 vs 常数」的形状不变**。

## 四、你的两个担心，分别怎么解

1. **「散落的文件太多、查询慢」** → 用 `index.db` 解决查询，**不是**换掉真相。
2. **「会不会散落太多数据库」** → ADR-002 的目录里**只有一个** `index.db`，放在数据根（`~/.relayer/index.db`），
   **不是每个 workspace 一个**；workspace 用一列区分。所以**不会散落**。

## 五、建议落地形态

```
~/.relayer/
├── workspaces/<ws>/.memory/
│   ├── atom-<slug>.md          ← 真相（git）
│   ├── MEMORY.md               ← 人的一行索引
│   └── .governance.jsonl       ← 治理审计线（append-only）
├── index.db                    ← 派生：唯一索引，可全量重建，可弃
└── kernel/kernel.db            ← 运行态（已有）
```

**不变量**（写进代码注释与测试）：

- 写路径 **先落文件、后刷索引**；索引刷新失败**不回滚文件**（索引可重建）。
- 读路径：需要**精确字节**的（`read_atom`、write 前的 sha256 闸）**只读文件**；
  索引用途仅限 `search` / `list` / `recall` / 聚合。
- `index.db` 可随时 `rm` → `relayer index rebuild` 从文件全量重建，结果与增量一致。
- 索引**不进 git**（`.gitignore`）。真相永远是文件。

schema 草稿：

```sql
CREATE TABLE atoms (
  slug TEXT PRIMARY KEY, type TEXT, granularity TEXT, about TEXT,
  namespace TEXT, grade TEXT, state TEXT, body TEXT,
  updated_at TEXT, sha256 TEXT
);
CREATE INDEX idx_ns_type_state ON atoms(namespace, type, state);
CREATE VIRTUAL TABLE atoms_fts USING fts5(slug UNINDEXED, body, tokenize='trigram');
```

（**边表先不建**：图谱仍按 ADR-004 决策 8 / design-conventions 在渲染期从 frontmatter 派生。）

## 六、什么时候建？沿用 ADR-023 决策 8 的判据

「`maxHits = 40` 开始频繁触发」是既定的物化信号。按上面的实测，这条信号会在
**远早于 5 万原子**的地方响；到 1 万时全扫已 ~0.9 s，就该建了。
所以：**判据不变，只是「提前触发」是可预期的。**

## 七、若仍决定走「纯数据库记忆」

那是推翻 ADR-002 决策 4。不是不能做，但代价要一次认账：

- 蒸馏 / 原子化管线从「写文件」改成「写 DB」（Claude 不天然查 sqlite）；
- 失去 `git log` / `git diff` 的记忆演化史，以及人工墓碑的天然落点；
- 引入 schema 与迁移，`kernel.db` 之外再多一套内容 schema；
- 必须自造审计线（ADR-033 要求 before/after + sha256 + reason + evidence）；
- 不再有「唯一真相」这一保护。

**建议：不做。** 用「文件真相 + `index.db` 派生」拿到你要的查询性能，同时保住 ADR-002 的全部好处。

## 附：复现方法

基准代码为一次性件（符合 ADR-025 的做法：跑完即删，不进长期代码），临时位于
`/tmp/rgl-bench`（`main.go`：全扫 vs 索引；`bench2/main.go`：细项）。依赖 `modernc.org/sqlite`（纯 Go）。

```bash
cd /tmp/rgl-bench && go run . && go run ./bench2
```
