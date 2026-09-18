-- 历史存档：memgov 旧记忆索引（index.db），不用于当前建库或升级。
--
-- 已被 memgov v2 取代：当前使用 Source、Candidate、Review、Memory，
-- SQLite state.db 是唯一真相源；0003 的记忆卡/战斗设计同样已归档。
-- 现行架构见 docs/architecture/architecture.md，Schema 由 internal/core 的版本迁移链维护。
-- 以下 DDL 和说明只保留当时记录，不是当前产品约束。
--
-- 旧版定位：派生层，可从 .memory/*.md 重建。当时采用文件真相源；v2 不采用。
--
-- 顶部不放 `PRAGMA foreign_keys = ON`：它是**连接级**设置，写在建表脚本里无效。
-- 每个连接都必须显式打开，否则 ON DELETE CASCADE 静默失效，
-- atom_about / atom_edges 会积累已删除原子的孤儿行（见 0002 第 5 节实测）。
-- Go 驱动应通过 DSN 参数开启：`file:index.db?_pragma=foreign_keys(1)`。

-- 元数据。source_root 用于防止 --data-dir 指错导致张冠李戴。
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- 原子主表。字段与 memory.Atom 一一对应，含归一的证据链（*_raw / *_issue）。
CREATE TABLE atoms (
  id                INTEGER PRIMARY KEY,
  workspace         TEXT NOT NULL,
  slug              TEXT NOT NULL,          -- CanonicalID 形态，保留 atom- 前缀
  description       TEXT NOT NULL DEFAULT '',
  type              TEXT NOT NULL,
  type_raw          TEXT NOT NULL DEFAULT '',
  type_issue        TEXT NOT NULL DEFAULT '',
  granularity       TEXT NOT NULL,
  granularity_raw   TEXT NOT NULL DEFAULT '',
  granularity_issue TEXT NOT NULL DEFAULT '',
  state             TEXT NOT NULL,
  state_raw         TEXT NOT NULL DEFAULT '',
  state_issue       TEXT NOT NULL DEFAULT '',
  kind              TEXT NOT NULL DEFAULT '',
  grade             TEXT NOT NULL DEFAULT '',
  namespace         TEXT NOT NULL DEFAULT '',
  from_turn         TEXT NOT NULL DEFAULT '',
  from_file         TEXT NOT NULL DEFAULT '',
  updated_at        TEXT NOT NULL DEFAULT '',
  body              TEXT NOT NULL DEFAULT '',
  sha256            TEXT NOT NULL,
  git_path          TEXT NOT NULL DEFAULT '',
  issues            TEXT NOT NULL DEFAULT '[]',   -- JSON 数组，证据而非查询维度
  extra             TEXT NOT NULL DEFAULT '{}',   -- JSON 对象，未知键原样保留
  UNIQUE (workspace, slug)
);

-- 召回热路径。列序 = 等值条件的固定性顺序：
-- workspace(必然等值) -> state(召回固定 active) -> namespace(IN 列表) -> type(可选过滤)
CREATE INDEX idx_atoms_recall  ON atoms (workspace, state, namespace, type);
CREATE INDEX idx_atoms_updated ON atoms (workspace, updated_at);
CREATE INDEX idx_atoms_turn    ON atoms (from_turn);

-- 指向轴（轴三）的多值展开。
-- 保留 ord 是因为「Render 必须是不动点」——顺序丢了重建会写出不同 diff；
-- 保留 raw 是因为「归一出词表的值不静默」，原始字面值本身是证据。
CREATE TABLE atom_about (
  atom_id  INTEGER NOT NULL REFERENCES atoms(id) ON DELETE CASCADE,
  ord      INTEGER NOT NULL,
  raw      TEXT NOT NULL,
  category TEXT NOT NULL DEFAULT '',
  name     TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (atom_id, ord)
);

CREATE INDEX idx_about_lookup ON atom_about (category, name, atom_id);

-- 溯源边。dst_slug **刻意不加外键**：悬空边是合法状态。
-- result_of 可能指向尚未写出的原子；强行外键会导致插入失败，
-- 或被迫为不存在目标建占位行（污染图谱）。悬空边交给 memory.validate 报 issue。
CREATE TABLE atom_edges (
  src_id   INTEGER NOT NULL REFERENCES atoms(id) ON DELETE CASCADE,
  kind     TEXT NOT NULL,          -- related / result_of / required_by / supersedes
  dst_slug TEXT NOT NULL,
  PRIMARY KEY (src_id, kind, dst_slug)
);

CREATE INDEX idx_edges_dst ON atom_edges (kind, dst_slug);

-- 全文索引。trigram 分词是中文的必需项（默认 unicode61 按整串切，中文基本不可用）。
-- description 一并索引：标题命中通常比正文命中更准。
-- external content 模式 = 不存第二份正文，靠 trigger 与 atoms 保持同步。
CREATE VIRTUAL TABLE atoms_fts USING fts5(
  description,
  body,
  content = 'atoms',
  content_rowid = 'id',
  tokenize = 'trigram'
);

CREATE TRIGGER atoms_ai AFTER INSERT ON atoms BEGIN
  INSERT INTO atoms_fts (rowid, description, body)
  VALUES (new.id, new.description, new.body);
END;

CREATE TRIGGER atoms_ad AFTER DELETE ON atoms BEGIN
  INSERT INTO atoms_fts (atoms_fts, rowid, description, body)
  VALUES ('delete', old.id, old.description, old.body);
END;

CREATE TRIGGER atoms_au AFTER UPDATE ON atoms BEGIN
  INSERT INTO atoms_fts (atoms_fts, rowid, description, body)
  VALUES ('delete', old.id, old.description, old.body);
  INSERT INTO atoms_fts (rowid, description, body)
  VALUES (new.id, new.description, new.body);
END;
