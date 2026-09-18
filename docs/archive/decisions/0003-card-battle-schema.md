# 历史存档 0003：记忆卡 / 战斗 表结构

> 已被 memgov v2 取代。下文的记忆卡、战斗、旧代码路径和命令只用于理解历史数据，不是当前实现或待实施方案。现行模型和命令见[架构](../../architecture/architecture.md)与[CLI 用法](../../../README.md)。

- 状态：**历史存档，不再适用**（原状态：设计已定，实现待触发）
- 日期：2026-09-11
- 当时取代：[`0002-index-schema.md`](0002-index-schema.md) 的表结构部分；两份文档现在均不约束 v2
- 数据源（导入源）：`~/.relayer/memories/*.md`（23 张）+ `~/.relayer/battles/*.json`（7 场）
- 旧版 DDL 路径：`internal/index/schema.sql`（已不是现行权威 Schema）
- 实现：`internal/index`（解析 + 重建）、`memgov index import` / `rebuild`
- 数据落点：见 [`0004-memgov-home.md`](0004-memgov-home.md) —— 库与源快照都落在 `~/.memgov`

## 结论

**两个对象 + 三种关系。**

| 对象 | 是什么 | 数量 |
|---|---|---|
| `memory_card` | 记忆卡。本机工作经历结晶成的结论 | 23 |
| `battle` | 战斗。一次执行任务的完整战报，是**复现单元** | 7 |

关系有三层，都落在表上：

1. **战斗装载了哪些卡** → `battle_card`（**带全文快照**，不是引用）
2. **战斗改动了哪些卡** → `battle_memory_change`（`create` / `revise` / `retire` …）
3. **卡与卡之间** → `memory_relation`（`follows` / `constrained_by` / `derived_from`）

外加一张 `evidence`（证据引用，贯穿全部对象）和一张 `search_fts`（统一检索面）。

## 1. 为什么推翻 0002

0002 的 atoms 三轴是**照抄 relayer-old 的 ADR 模型**。对照本机真实数据，两者不是一套东西：

| 维度 | 0002（照抄 relayer-old） | `.relayer/` 真实在跑 |
|---|---|---|
| 核心对象 | 只有"原子" | **记忆卡 + 战斗**两个 |
| 性质轴 | `type` = fact/decision/rule/experience/requirement/todo | `card.type` = fact / playbook / decision / episode |
| 粒度轴 | `granularity` = atom/process | **不存在** |
| 关系 | `from_turn` / `result_of` / `required_by` | `relations.{follows,constrained_by,derived_from}` + `evidence` 前缀引用 |
| 复现 | 无此概念 | 战斗快照，是核心能力 |

**0002 里继续有效的部分**（不重写）：

- 派生层定位：文件是真相，库可 `rm` → rebuild
- 写路径先落文件后刷索引；索引失败不回滚文件
- 精确字节只读文件；索引只管 `search` / `list` / `recall` / 聚合
- `PRAGMA foreign_keys` 每个连接必须开
- FTS 用 trigram；派生层不做 migration
- 落地顺序：先只做 rebuild，再切读路径，最后接增量

被取代的只有**表结构**。

## 2. 表结构总览

| 表 | 装什么 | 归属 |
|---|---|---|
| `memory_card` | 卡本体：title / summary / body / type / scope / state / version / **origin** | 卡 |
| `memory_card_tag` | **卡级**指向标签（`about`） | 卡 |
| `memory_relation` | 卡→卡关系（含悬空目标） | 卡 |
| `memory_card_issue` | 卡的问题（自带 `classification.issues` + 本层兜底动作） | 卡 |
| `battle` | 战报本体：套牌定义 / 结果 / 结算 | 战斗 |
| `battle_card` | **装载快照**：当时用的哪一版、内容是什么 | 战斗 |
| `battle_card_tag` | 快照内的指向标签（`person:` / `system:` / `topic:` / `event:`） | 战斗 |
| `battle_next_deck` / `battle_next_card` | 结算给出的下一场建议套牌 | 战斗 |
| `battle_memory_change` | **战斗对卡的更新与淘汰** | 战斗→卡 |
| `battle_event` | 战斗内事件流（`request`/`tool`/`approval`/`verification`/`reply`/`memory`/`note`） | 战斗 |
| `battle_issue` | 战报解析期发现的问题（与 `memory_card_issue` 对称，不静默） | 战斗 |
| `evidence` | 多态证据引用（30 种 scheme） | 全部 |
| `search_fts` | 统一检索面（title / summary / body） | 全部 |
| `meta` | schema 版本 / 数据根 / 重建时间 / 卡数拆分 | 索引自身 |

共 **14 张普通表 + 1 张 FTS5 虚拟表（`search_fts`）/ 16 个索引 / 6 个 trigger**。

## 3. 六个由实测决定的判断

### 3.1 战斗快照必须存全文，不能只存 `card_id`

实测：战斗引用过的 15 张卡里，**9 张已不在 `memories/` 下**（全是 `mem_<uuid>` 流水线临时卡）。

> **补充（2026-09-11，范围扩大后）**：这 9 张**并没有丢** —— 它们在
> `imports/legacy-*/memories/` 的归档里，纳入索引后引用解析率 **15/15**。
> 但「快照存全文」的决策**不变**，理由更硬：归档本身也是可被清理的一层，
> 快照不欠任何外部目录的人情。这是纵深防御，不是补救措施。

### 3.2 `card_id` 绝不能加外键

同上。`battle_card.card_id`、`battle_memory_change.memory_id`、`battle_card_tag.card_id`
都是"指向可能已经不存在的东西"，加外键会让写入直接失败。
和 `memory_relation.dst_id` 一样，**悬空是合法状态**，交给 `memory validate` 报 issue。

### 3.3 `about` 只在快照里有（**结论已被修正，见下**）

实测三步，结论一致：

- 23 张卡的 md 文件里 **0 张**带 `about`
- 非空 `about` 只在 battle 快照里，共 **12 处**
- 这 12 处**全部**挂在 `mem_*` 卡上，**而这批卡的文件已全部消失**

曾按「`memory_card_tag` + 外键指向 `memory_card`」设计，实测该表被过滤成 **0 行**。
改为挂在 `battle_card` 上之后，`battle_card_tag` 得到 38 行，反查正常：

```
person:周守健 → mem_00f6fa0a… / mem_1ffd37c6… / mem_2f094fb1…   （卡状态：源已消失）
```

#### ⚠️ 修正（2026-09-11，范围扩大后发现）

上面的样本**只覆盖了 `memories/` 下的 23 张在用卡**。扩大到归档后结论翻转：

- `imports/legacy-<dataset>/memories/*.md` 有 **661 张归档卡**，与在用的 23 张**零交集**（合计 684）
- 这 661 张里 **650 张带 `about`**（写的是 `about: &a1` + 序列，YAML 锚点）

所以 **`about` 确实是卡级属性**，不是快照专有 —— 只是在用的 23 张恰好没写。

当前表结构**没有承载卡级 `about` 的地方**（`battle_card_tag` 挂在快照上）。
这不是 bug，是**索引范围**造成的：索引只圈了 `memories/` + `battles/`。
一旦把 661 张归档卡纳入范围，就需要补一张 `memory_card_tag`（或把 `about` 落到 `memory_card`）。
**在此之前保持现状**，避免为范围外的数据改表。

范围决策见本文档末节「9 索引范围与已知缺口」。

### 3.4 `search_fts` 用普通表（跨对象），必须自己写同步 trigger

0002 的 FTS 用 external content 模式，靠 `atoms` 表自动跟随。
0003 要跨「记忆卡 / 战斗」两个对象统一检索，**挂不到单一内容表**，只能用普通 FTS 表 —— 代价是自己维护。

实测踩到的坑：导入器手写了一次 `INSERT INTO search_fts`，而 `memory_card_ai` trigger 也插一次，
结果 `search_fts` 是 **60 行**（应为 30）。规则定死：

- **应用层永远不碰 `search_fts`**，只由 trigger 写入 —— 全量重建与增量写入都一样
- 落地：`internal/index` 的插入语句里没有任何 `search_fts`；
  `index_test.go` 断言 `count(search_fts) == 卡数 + 战斗数`，双写会让它翻倍

### 3.5 `evidence` 是多态表，外键管不到，必须用 trigger 清理

`owner_kind` 是运行时值（`memory_card` / `battle` / `battle_event` / `battle_change` / …），
SQLite 无法用外键表达这种多态引用。删除时若不清，会留下查得到但已无主的证据行。

`memory_card_ad` / `battle_ad` 两个 trigger 负责清 `search_fts` 与 `evidence`。

### 3.6 战报 JSON 必须宽容解析：一个字段类型漂移 = 整场战斗消失

**这是跑真实数据时才炸出来的坑，也是本设计里代价最高的一次错误。**

最初 `promotion.mergedCards` 按 `[]string`（卡 id 列表）声明。真实数据里它是**计数**：

```json
"promotion": { "status": "verified", "mergedCards": 0 }
```

`json.Unmarshal` 撞到类型不符就返回错误，`ParseBattle` 顺势把整场战斗丢弃 ——
**7 场里 5 场直接消失**（只剩 2 场 / 4 个快照 / 0 个标签）。
更坑的是它不崩、不报错，只是静默少数据：`battles: 2` 看起来像个正常数字。

定死两条：

1. **战报的所有叶子字段走宽容取值器**，永不返回错误：
   `jsonString` / `jsonInt` / `jsonFloat` / `jsonBool` / `jsonStrings` / `jsonObjects[T]`
   - 数组元素取不出标量就跳过，不毁掉整段
   - 单标量写成裸值也当单元素数组收下
   - `int` / `float` / `"3"` / `"v1"` 都能取成数或串
2. **Parse 对语法合法的 JSON 永不失败**：取值漂移用可取到的值建战斗，
   原因写进 `Battle.Issues` → `battle_issue`（与 `memory_card_issue` 对称，不静默）。
   连 JSON 语法都不成立时，仍返回一个只带 `id` / `path` / `sha256` 的**壳**并入库 ——
   战报是"本机发生过这件事"的证据，整体消失比字段缺值严重得多。

一个关键实测事实支撑上面的第 2 条：**Go 的 `encoding/json` 碰到字段类型不符时会继续解完其余字段**，
只在最后返回第一个错误。所以残值是可用的，丢整场纯属自伤：

```go
// {"first":"F","bad":0,"third":"T","nested":{"a":7},"last":"L"}
// bad 声明为 []string（真实是 int）
err   = json: cannot unmarshal number into Go struct field probe.bad of type []string
first="F"  third="T"  nested.A=7  last="L"   // ← 其余字段全对
```

`SchemaVersion` 因此从 `1` 升到 `2`：`promotion_merged`（JSON 数组）→
`promotion_merged_cards`（计数），并新增 `battle_issue`。之后扩范围又升到 `3`
（`memory_card` 加 `origin`/`archive_ref`，新增 `memory_card_tag`）。
派生层不做 migration，升版号即提示删库重建。

## 4. summary 先行：两阶段检索

你要求「两个对象都支持 summary，让 AI 快速知道，要详情再取更多」。落法：

```sql
-- 阶段 1：只命中摘要层，快速回答「有没有这回事」
SELECT f.kind, f.ref_id, f.title, f.summary
FROM search_fts f
WHERE search_fts MATCH ?
ORDER BY bm25(search_fts, 5.0, 10.0, 1.0)   -- title / summary / body
LIMIT 20;

-- 阶段 2：命中后再按 ref_id 取全文
SELECT body FROM memory_card WHERE id = ?;
```

`bm25` 的权重按列序给，**summary 拿到最高权重 10.0**（title 5.0、body 1.0），
所以摘要命中的结果稳定排在前面。实测查「白名单」时首条即 battle 战报，其后是卡，符合预期。

## 5. 验证结果（真实数据，非构造）

### 5.1 设计期：DDL 直接灌真实数据

23 张卡 + 7 场战斗 + 自动生成的 1069 行 SQL，零错误导入（`PRAGMA foreign_keys` 开启）。

| 项 | 结果 |
|---|---|
| 计数 | card 23 / battle 7 / 快照 21 / 事件 38 / 变更 6 / 标签 38 / 证据 248 / FTS 30 |
| summary 先行检索 | 「白名单」→ 战报优先，其后 5 张卡 |
| 复现 | `zz-net-whitelist-2026-09-03` 装载 v1，当前 v3 → **OUTDATED** |
| 战斗改动卡 | 6 条：`create` ×4、`revise` ×2，均带 commit |
| 只活在快照里的卡 | 9 张，全部可查 |
| 标签反查 | `person:周守健` → 8 条，卡状态全为「源已消失」 |
| 级联（删卡） | relation / issue / FTS / 证据 全清；快照保留 |
| 级联（删战斗） | 快照 / 事件 / 变更 / 标签 / 证据 全清 |

库体积 788 KB。

### 5.2 实现期：`memgov index rebuild` 端到端

扩范围前（只扫 `memories/` + `battles/`，SchemaVersion 2）：

```console
$ memgov index rebuild
{ "cards": 23, "battles": 7, "snapshots": 21, "changes": 6,
  "events": 38, "tags": 38, "evidence": 248,
  "battle_issues": 0, "elapsed_ms": 64 }
```

扩范围后（含 661 张归档卡，SchemaVersion 3）：

```console
$ memgov index rebuild
{ "cards": 684, "cards_active": 23, "cards_archived": 661, "battles": 7,
  "snapshots": 21, "changes": 6, "events": 38,
  "card_tags": 1950, "tags": 38, "evidence": 276,
  "battle_issues": 0, "card_issues": 413, "elapsed_ms": 506 }
```

零 `problems`、零 `battle_issues`。额外核验（全部通过）：

| 核验 | 结果 |
|---|---|
| 源↔库集合 | 684/684，无缺失无多余 |
| sha256 逐文件核对 | 684/684 命中 |
| `origin` 归属 | 0 错配；无 `archive_ref` 缺失 |
| 卡级 `about` | 源 1950 条 == 库 1950 条，**逐卡 0 不一致** |
| `state` 分布 | active 644 / stale 30 / unknown 10（与源一致） |
| `type` 分布 | fact 306 / playbook 225 / decision 67 / episode 52 / unclassified 34 |
| `originals/` 泄漏 | 0（旧 atom 格式未被当作卡索引） |
| 检索面行数 == 卡数 + 战斗数 | 691 == 684 + 7（无双写） |
| 证据属主可解析（6 类 `owner_kind` 逐类反查） | 0 孤儿 |
| 级联后孤儿子行 | 0 |
| **战报引用卡的解析率** | **15/15**（扩范围前 9 张查不到） |
| 幂等性 | 两次重建 6,960 行逐行一致（已排除 `meta`） |

耗时 506ms / 684 张卡 ≈ 0.74ms 每张 —— 瓶颈仍是 YAML 解析（`about` 是块序列，
归档卡 frontmatter 比在用卡厚），与 0001 的基准结论一致。

## 6. 一个反复踩的坑

`PRAGMA foreign_keys` **默认 OFF 且是连接级**。

- 写在建表脚本里 → 无效
- 0002 验证时踩过一次（`about` 残留 1 行）
- 0003 验证时**又踩一次**（重写验证脚本时漏了那行，删战斗后快照/事件/变更全部残留）

两次都**没有任何报错**，查询看起来照常工作。这条必须靠自动化测试兜住：**重建后加一条删-查断言**。

## 7. 落地顺序

沿用 0002，不变：

1. **只做 `index rebuild`**：全量扫 `memories/*.md` + `battles/*.json` → 建库。不接写路径。
   此时索引是纯只读快照，错了重跑就是，风险为零。
2. 读路径切到索引，对照测试「索引结果 ≡ 文件全扫结果」。
3. 最后接增量：写文件成功后 upsert 索引（走 trigger）。

## 8. 上游数据问题（建议反馈给 relayer-next）

设计过程中发现 6 处真实不一致，都不是本项目的 bug，但会影响索引正确性：

1. **`version` 表示法不一致**：md 写整数 `version: 3`，battle 写字符串 `"v1"`。入库统一成十进制串。
2. **`about` 疑似丢字段**：它语义上是卡的指向标签，却只在快照里存在，md 里 0/23。
   若本应写进 md，那是上游的丢失（和 ADR-004 的 `supersedes` 被静默丢弃同类）。
3. **9 张 `mem_*` 卡被战斗引用但文件已删**：临时卡的淘汰/归档策略不明确。
   索引侧已按「悬空合法」处理，但上游最好是显式墓碑而不是直接消失。
4. **`evidence` 格式松散**：存在含空格的 scheme（`relayer-next git commit 5301f16a…`）、
   中文全角冒号（`DWS CI/CD问题反馈群：刘荣涛…`）、以及无冒号的裸串。
   解析必须宽容（无冒号时 scheme 留空），不能假设 `scheme:ref` 严格成立。
5. **`schema_version` 只有 3/23 有**：字段后加，旧卡缺省。入库允许 NULL。
6. **`promotion.mergedCards` 语义与命名不符**：字段名像卡 id 列表，实际是**计数**
   （`"mergedCards": 0`）。建议改名 `mergedCardCount` 或真的写成 id 数组 ——
   当前这个名字诱导了本项目的实现错误（见 3.6），已按计数落库。

## 9. 代码落地

DDL 的唯一权威副本在**代码目录**（用 `//go:embed` 引用），本文档只做解释，
`docs/archive/decisions/0002-index-schema.sql`（原位于旧 decisions 目录） 那种双份 SQL 不再重演：

| 文件 | 职责 |
|---|---|
| `internal/index/schema.sql` | 权威 DDL（14 普通表 + 1 FTS 虚拟表 / 16 索引 / 6 trigger） |
| `internal/index/model.go` | 领域模型 + 宽容取值器（`versionValue` / `yamlStrings` / `jsonInt` / …） |
| `internal/index/parse.go` | `ParseCard` / `ParseBattle` / `LoadCards` / `LoadSourceCards` / `LoadBattles` / `SplitEvidence` |
| `internal/index/rebuild.go` | `Rebuild`（临时库 + 原子替换）、外键断言、报表 |
| `internal/index/insert.go` | 全部 SQL 常量与写入（**不碰 `search_fts`**） |
| `cmd/memgov/index.go` | `memgov index rebuild --source <dir> --db <path>` |

不变量都有测试兜住（`index_test.go`）：

| 测试 | 守什么 |
|---|---|
| `TestRebuildEndToEnd` | 端到端计数、快照全文、summary 检索、promotion 列语义、`battle_issue=0`、**origin 归属、卡级 about、unclassified、`originals/` 不泄漏** |
| `TestRebuildIsIdempotent` | 两次全量重建逐行一致（"索引是派生"的证据） |
| `TestForeignKeysEnabled` / `TestCascadeOnDelete` | `PRAGMA foreign_keys` 真的开着，级联不留幽灵行（含 `memory_card_tag`） |
| `TestParseCardAboutForms` | `about` 三种真实写法：内联序列 / 锚点块序列 / 单标量 |
| `TestParseCardUnclassified` | 缺 `card.type` 夹回 `unclassified` 且留 issue；`state: unknown` 原样保留 |
| `TestLoadSourceCardsOrderAndScope` | active 在前 + 归档按数据集排序；`originals/` 一张都不进；无 `memories/` 的归档目录跳过而不报错 |
| `TestParseBattleToleratesRealShapes` | 把真实漂移形态全塞进一份战报：int `mergedCards`、int `version`、裸标量数组、非对象事件元素、字符串 `promote` |
| `TestParseBattleOnBrokenJSONKeepsShell` | 语法坏掉仍留 `id`/`path`/`sha256` 壳 |
| `TestRebuildKeepsBrokenBattle` | 端到端：坏战报仍入索引且进检索面，原因进 `problems` |
| `TestJSONTolerantValues` | 取值器边界（`"v4"` 取不到数、对象元素跳过、`"true"` 认成布尔…） |
| `TestSplitStatementsKeepsTriggersWhole` | trigger 体不被 `;` 从中间切断 |

## 10. 索引范围（已扩到归档）

**索引现在的范围是：`memories/` + `imports/legacy-*/memories/` + `battles/`。**
依然**不是**整个 `~/.relayer`（3269 文件 / 52.3 MB），但卡已经全覆盖。

| 对象 | 位置 | 数量 | 在索引里 |
|---|---|---|---|
| 记忆卡（在用） | `memories/*.md` | **23** | ✅ |
| 记忆卡（归档） | `imports/legacy-<dataset>/memories/*.md` | **661** | ✅ |
| 战斗 | `battles/*.json` | **7** | ✅ |
| 归档原件（旧 atom 三轴） | `imports/legacy-*/originals/**/.memory/*.md` | 661（603 旧格式） | ❌ **故意不索引** |
| scene 引用 | `scene-references/*.json` | 132 | ❌ |
| hero 分类 | `heroes/*.json` + `heroes/runs/` | 32 | ❌ |
| maintenance 提案 | `maintenance/*.json` | 20 | ❌ |
| knowledge deck / 合并 | `knowledge-decks/**` | 9 | ❌ |
| operations 记录 | `operations/*` | 10 | ❌ |
| sop-migrations / merge-scan / events / ledger / scheduled-maintenance | 各 1–2 | 6 | ❌ |
| 运行时库 | `state.sqlite`(31M) / `channels.sqlite`(5M) / `scene-index.sqlite` / `local-directories.sqlite` / `task-closeouts.sqlite` / `schedule.sqlite` / `external-knowledge.sqlite` | — | ❌ |

`originals/` 为什么**故意**不索引：它是同一批内容的**旧 atom 三轴格式**副本
（`granularity:` / `namespace:` / `from_turn:` / `derived_from:`）。读进来只会产生
重复条目与"同一件事两个模型"的语义歧义。测试 `TestLoadSourceCardsOrderAndScope`
用 `id LIKE 'atom-%'` 断言它一张都没漏进来。

### 卡的完整宇宙是 684 —— 现已全部入库

```
在用 memories/            23   → origin=active
归档 imports/.../memories/ 661   → origin=archive, archive_ref=legacy-0095edbadefad6c05b999288
                         -----
合计（零交集）            684   ← 覆盖 684/684 = 100%
```

旁证：`state.sqlite` 的 `card_view_state` 有 **680 行**，是 684 的子集。

**为什么用 `origin` 列而不是合并目录**：归档是显式的历史层，"检索默认只看在用的"
必须能一键过滤（`WHERE origin='active'`）。实测两张来源的 id **零交集**，
所以共用 `memory_card.id` 主键成立；日后若出现交集，就该改成复合主键而不是先冒险。

归档卡格式与在用卡**完全一致**（`schema_version: 1` + `card.type`），就是早期
`mem_*` 批量卡的归档。`imports/legacy-<dataset>/manifest.json` 记录了来龙去脉：

```
records 661 / typedCards 627 / pendingReview 245 / usable 408 / workspaces 24
types: fact 298, playbook 218, decision 62, episode 49, 未分类 34
issueCounts: missing-title 44, review-rule-semantics 78, review-experience-semantics 49, legacy-yaml-invalid 33
```

### 扩范围时的三个决策（已落地）

| 问题 | 决策 | 落点 |
|---|---|---|
| 650/661 张带 `about`，表无处承载 | 新增 **`memory_card_tag`**（与 `battle_card_tag` 对称） | `schema.sql` |
| `state: unknown`（10 张）是枚举外值 | 当作**写方产出的真实值**收进词表 `StateUnknown`，保留原样、不报 issue | `model.go` |
| 34 张没有 `card.type` | 夹回自解释兜底值 **`unclassified`** + 原因写进 `memory_card_issue`（不留空串，否则"未分类"与"解析失败"无法区分） | `parse.go` |

`about` 的三种写法都要收：内联序列 `[a, b]`、YAML 锚点 + 块序列 `&a1` + `- x`、单标量。
标准库只认第一种，所以用 `yamlStrings` 宽容取值器（`model.go`），
和 `versionValue` 同一个理由。

**注意 `card_issues = 413` 大部分不是本层产生的**：`missing-title 44`、
`review-rule-semantics 78`、`legacy-yaml-invalid 33` 等来自卡自带的
`classification.issues`（上游分类流水线的结论），本层只是转发。
本层自己只加两类：缺 `card.type`（34 条）与缺 frontmatter `id`（实测 0 条）。

### 结论

范围扩大**不改变 0001**：文件仍是唯一真相，库仍是派生索引，
改的只是 `Rebuild` 扫哪些目录（+ 表结构加 `origin` 与卡级标签）。

剩余未索引的对象（scene / hero / maintenance / 运行时 sqlite）**按需再加**，
每类都要先问一遍「它是不是同一个对象模型」—— 不要因为"都在 `~/.relayer` 下"就塞进同一张表。

> **后续（0004）**：本文件的表结构一字未改，但**扫的目录换了**。
> `Rebuild` 现在扫的是 memgov 自有快照 `<home>/sources/<id>/`（默认 `~/.memgov/sources/relayer/`），
> `~/.relayer` 降级为 `memgov index import` 的导入源。见
> [`0004-memgov-home.md`](0004-memgov-home.md)。
