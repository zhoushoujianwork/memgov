package cli

import (
	"context"
	"github.com/spf13/cobra"
	"github.com/zhoushoujianwork/memgov/internal/core"
	"os"
	"path/filepath"
)

func (a *app) backupCommands() {
	backup := &cobra.Command{Use: "backup", Short: "一致性 SQLite 备份与保留墓碑的恢复"}
	var output string
	create := a.read("create", "创建可校验的独立数据库快照", cobra.NoArgs, func(ctx context.Context, s *core.Store, _ []string) (any, error) {
		if a.key != "" && output != "" {
			return nil, core.Fail("invalid_input", "keyed backups use the default backup directory; omit --output")
		}
		path := output
		if path == "" {
			name := core.NewID()
			if a.key != "" {
				name = core.Hash([]byte(a.key))
			}
			path = filepath.Join(a.home, "backups", name+".db")
		}
		if a.key != "" {
			if _, err := os.Stat(path); err == nil {
				return core.VerifyBackup(ctx, path)
			}
		}
		return s.CreateBackup(ctx, path)
	})
	create.Flags().StringVar(&output, "output", "", "新备份文件路径，默认放入 home/backups")
	backup.AddCommand(create)
	backup.AddCommand(a.simple("list", "检查并列出本地备份", cobra.NoArgs, func(ctx context.Context, _ []string) (any, error) {
		return core.ListBackups(ctx, filepath.Join(a.home, "backups"))
	}))
	backup.AddCommand(a.simple("verify <file>", "校验备份完整性、Schema 与指纹", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) { return core.VerifyBackup(ctx, args[0]) }))
	var digest string
	restore := a.simple("restore <file>", "在隔离库校验与重施墓碑后原子恢复", cobra.ExactArgs(1), func(ctx context.Context, args []string) (any, error) {
		return core.RestoreBackup(ctx, a.dbPath(), args[0], digest, core.Request{ID: a.requestID, Command: "backup.restore", Scope: "global", Actor: a.actor, Key: a.key, Input: map[string]string{"file": args[0], "digest": digest}})
	})
	restore.Flags().StringVar(&digest, "expected-digest", "", "verify 返回的备份 SHA-256")
	backup.AddCommand(restore)
	a.root.AddCommand(backup)
	a.root.AddCommand(a.read("export", "导出完整 JSON 交换包或可阅读 Markdown", cobra.NoArgs, func(ctx context.Context, s *core.Store, _ []string) (any, error) {
		p, err := core.Export(ctx, s)
		if err != nil {
			return nil, err
		}
		if a.format == "markdown" {
			return p.Markdown()
		}
		return p, nil
	}))
}
