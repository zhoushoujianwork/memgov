package tasklog

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func maintain(root string) {
	type item struct {
		path     string
		size     int64
		modified time.Time
	}
	var files []item
	var total int64
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.IsDir() || filepath.Ext(path) != ".jsonl" {
			return nil
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		if time.Since(st.ModTime()) > Retention {
			_ = os.Remove(path)
			return nil
		}
		total += st.Size()
		files = append(files, item{path, st.Size(), st.ModTime()})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].modified.Before(files[j].modified) })
	for _, f := range files {
		if total <= 128<<20 {
			break
		}
		if time.Since(f.modified) < time.Hour {
			continue
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
}
