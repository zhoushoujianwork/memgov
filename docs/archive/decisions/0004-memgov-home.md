# 历史存档 0004：数据归属 —— 库落 memgov 自己的家目录

> 已被 memgov v2 取代。下文的 `memgov.db`、文件快照真相源及旧索引命令仅为历史记录。当前权威库是 `state.db`，不能删除后靠重建索引恢复；见[初始化](../../guides/initialization.md)、[现行架构](../../architecture/architecture.md)与[CLI 用法](../../../README.md)。

- 状态：**历史存档，不再适用**（原状态：旧版已实施）
- 日期：2026-09-11
- 当时上游：[`0001-memory-storage.md`](0001-memory-storage.md)；文件真相 / 派生库的旧结论已被 v2 取代
- 实现：`internal/index/import.go`、`internal/index/status.go`、`cmd/memgov/index.go`
- 命令：`memgov index import` / `rebuild` / `status`

## 结论

**memgov 的数据全部落在自己的数据根下，不再与外部目录共用。**

```
~/.memgov/                              默认；可用 MEMGOV_HOME 覆盖
├── memgov.db                           库（派生层，可 rm → rebuild，不进 git）
└── sources/<id>/                       导入快照（files-as-truth 的新家，可进 git）
    ├── .memgov-snapshot.json           归属标记 + 来源记录
    ├── memories/*.md                   在用卡
    ├── battles/*.json                  战斗
    └── imports/legacy-*/memories/*.md  归档卡
```

外部目录（`~/.relayer`）从**运行期依赖**降级为**导入源**：`import` 之后，
查询与重建都只读 `~/.memgov`，外部目录可以整个删掉。

## 1. 问题：改造前共用的是什么

改造前 `memgov index rebuild` 直接读 `~/.relayer`（并把它写进 `meta.source_root`），
库则放在仓库里的 `./.memgov/data/index.db`。两处都别扭：

| 现象 | 为什么是问题 |
|---|---|
| 读 `~/.relayer` 才建得出库 | `~/.relayer` 是 **relayer-next 的活跃运行时目录**（63 MB，`state.sqlite` 一直在写，还有 `channels.sqlite` / `task-closeouts.sqlite` 等 15 个自己的库）。memgov 的读路径挂在一个别人的活目录上，对方一改布局就断线 |
| 库在仓库里 | 库是可弃的派生层，却和源码同住，`git status` 天天多一个 4.6 MB 的未跟踪文件 |
| 无来源记录 | 库重建之后无从回答「这批 684 张卡是从哪来的、什么时候来的、有没有变过」 |

## 2. 设计

### 2.1 三层，都在自己家

| 层 | 位置 | 性质 |
|---|---|---|
| 源快照（真相） | `<home>/sources/<id>/` | 文件，可 `git`，出了事能读能改 |
| 库（派生） | `<home>/memgov.db` | 可 `rm` → `rebuild`，不进去 |
| 外部源 | `index.source_dir`（默认 `~/.relayer`） | 只在 `import` 时读，**全程只读** |

快照标识 `<id>` 由源目录名推出（`~/.relayer` → `relayer`），
非 `[A-Za-z0-9_-]` 字符折成 `-`。用目录名而不是哈希：目录要能一眼看出数据从哪来。

> **命名说明**：0001 / 0002 / 0003 里写的 `index.db` 是**泛指**「派生层的那个库」，
> 结论不受影响。本项目自有库的文件名取 `memgov.db` —— 它落在 `~/.memgov/` 下，
> 名字要能自证归属，而不是让人以为是个通用索引文件。

### 2.2 快照的同步语义是**镜像**，不是叠加

`import` 走 `planSource` → `syncSnapshot` → `Build`：

1. **只收会被索引进库的那部分**：`memories/*.md`、`battles/*.json`、`imports/legacy-*/memories/*.md`。
   归档目录下的 `originals/` **明确不收** —— 那是卡所依据的原始资料，
   定位文档写死「资料留在原处，只留引用，不复制全文」；而且它是旧 atom 三轴格式的同一批内容，
   `LoadSourceCards` 本来就不读，收进快照只会白占空间。实测：`imports/` 下 661 份 `originals/`
   与 661 张卡同在，只搬了后者。
2. **镜像**：源里没有的文件从快照删掉，空目录一路清上去。
   否则源里删一张卡、库里还留着，两边悄悄漂开且无从察觉。
3. **内容未变（sha256 相同）不重写**：重复导入不产生无意义 diff，快照因此可以放心进 git。

**防误删**：镜像语义是破坏性的，所以快照根必须有 `.memgov-snapshot.json` 标记。
目录非空且没有标记时**直接拒绝**，一个字都不写 —— 挡住「`--snapshot-dir` 指到家目录」这类事故。

### 2.3 溯源写进 `meta`，不新开表

```
import_source_root    $HOME/.relayer               数据从哪个外部目录来
import_dataset_id     relayer                      快照标识
import_snapshot_dir   $HOME/.memgov/sources/relayer
imported_at           2026-09-11T18:54:52+08:00
import_file_count     691
import_bytes          1560253
import_digest         7e527d2e…                    全量内容指纹，用于发现漂移
```

**为什么放 `meta` 而不是新表**：`meta` 本来就是 key/value，够用；
而新开表要把 `schema_version` 从 3 抬到 4，派生层又**不做 migration**，
等于让所有人的旧库强制重建一次 —— 代价与收益完全不成比例。

额外键与核心键（`schema_version` / `card_count` / …）冲突时**核心键优先**：
调用方不该能用 `ExtraMeta` 改写事实。这条在 `TestImportSnapshotsIntoOwnHome` 里有断言。

### 2.4 「库在哪、从哪来、装了什么」要能一句话问出来

新增 `memgov index status`，同时给两个来源：

- `meta`：上次重建**写下**的计数与溯源（历史）
- `counts`：各表**现场** `count(*)`（当下）

两边都看是刻意的：库被手工改过或写到一半断电，`meta` 会继续撒谎，
而「meta 与现场对不上」本身就是信号。

## 3. 没有改的事：文件仍是真相，库仍是派生

用户在先前的选择里点过「改为 DB 当真相」。**这一条没有实施，0001 继续有效。** 理由不是保守：

**memgov 对这 684 张卡 + 7 场战斗不是写方。**

| 对象 | 谁写 | memgov 的角色 |
|---|---|---|
| `~/.relayer/memories/*.md`（23 张在用卡） | relayer-next | **只读** |
| `~/.relayer/imports/legacy-*`（661 张归档卡） | 一次性 legacy 导入 | **只读** |
| `~/.relayer/battles/*.json`（7 场） | relayer-next | **只读** |
| `<workspace>/.memory/atom-*.md` | memgov 自己 | 读写（但用的是另一套三轴模型） |

把库升格成真相，等于让 memgov 把这些**它写不出来、也回写不进去**的数据宣告为自己的唯一副本：
真相仍在别人文件里，memgov 只是多了一份会漂的二手拷贝 —— 这恰恰是 ADR-002 当初否掉纯 DB 的理由。

**要真的翻成 DB 当真相，得先做两件事（都还没做）**：

1. **接管写侧**：relayer-next 停止写 `~/.relayer/{memories,battles}`，或让 memgov 成为唯一写方；
2. **换掉落点**：新卡的落点从 relayer-next 的文件改成 memgov 的库，并给出等价的
   `git diff` 演化史与人工纠错路径（ADR-033 的审计线要求 before/after + sha256 + reason + evidence）。

在这两件落地之前，本设计取的是**两者兼得**的形态：
库是 memgov 自己家目录里的资产（用户要的那一点），
同时保留「删库 → 从快照重建」这条 0001 的不变量（不制造二手真相）。

## 4. 验证（真实数据，2026-09-11）

```
$ memgov index import
source_dir    $HOME/.relayer
snapshot_dir  $HOME/.memgov/sources/relayer
db_path       $HOME/.memgov/memgov.db
files 691  bytes 1560253  copied 691  unchanged 0  removed 0
digest        7e527d2e9fe441e2ad399b68b0eefe07e058a3c3c7491c2e97ba7069be307fca
index: cards 684 (active 23 / archived 661)  battles 7  card_tags 1950
       relations 632  evidence 276  fts 691  battle_issues 0  elapsed 456ms
```

| 检查 | 结果 |
|---|---|
| 二次 `import` | `copied 0 / unchanged 691 / removed 0`，digest 不变 |
| 快照 vs 源文件数 | 691 = 23 + 7 + 661；`originals/` 未混入（0 个） |
| 权限 | `~/.memgov`、`sources/*` 均 `0700`，文件 `0600` |
| `memgov index rebuild`（从快照） | 684 / 7，计数与导入时一致 |
| 导入库 vs 重建库逐表比对 | **14/14 张表内容 sha256 完全相同**（含 `search_fts`） |
| `~/.relayer` 是否被写过 | 无任何文件 mtime 晚于导入时刻 |
| `make check` | fmt-check + vet + test 全过（新增 6 个用例） |

## 5. 未做 / 待定

1. **`~/.memgov` 还没有自己的 git**。0001 要的「`git log` = 记忆演化史」在快照这一层还没兑现。
   快照已经可以被 git（镜像 + 内容不变不重写 = diff 干净），但要不要在用户家目录里
   `git init` 是个要单独拍板的事，没有擅自动手。
2. **外部源的增量变化靠重跑 `import`**。目前是全量镜像 + sha 比对，
   691 个文件约 0.5 s；量级再上一个数量级时需要改成按 mtime/size 预筛。
3. **写侧归属未定**（见第 3 节）。它是「DB 当真相」的前置条件，不是本文件的遗留项。
4. **读路径还没切到库**。当前所有读仍走文件全扫；库只被 `status` 读。
   切换（`search` / `list` / `recall` 走库 + 「索引结果 ≡ 文件全扫结果」对照测试）是下一步。
