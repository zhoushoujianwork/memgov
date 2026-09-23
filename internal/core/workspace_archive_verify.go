package core

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Verification reads the completed archive back, so successful source reads or
// a checksum of the container alone cannot mask a missing or corrupt entry.
func verifyLegacyKnowledgeArchive(ctx context.Context, path, databaseSHA string, expected *knowledgeArchiveManifest) error {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 1024 || info.Size()%512 != 0 {
		return Fail("invalid_input", "incomplete legacy knowledge archive")
	}
	end := make([]byte, 1024)
	if _, err = f.ReadAt(end, info.Size()-1024); err != nil || !bytes.Equal(end, make([]byte, 1024)) {
		return Fail("invalid_input", "legacy knowledge archive has no complete trailer")
	}
	reader := tar.NewReader(f)
	entries := map[string]knowledgeArchiveEntry{}
	var manifest *knowledgeArchiveManifest
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		header, e := reader.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
		if header.Name == "archive-manifest.json" {
			if manifest != nil || header.Typeflag != tar.TypeReg || header.Size > 64<<20 {
				return Fail("invalid_input", "invalid legacy knowledge archive manifest")
			}
			manifest = &knowledgeArchiveManifest{}
			decoder := json.NewDecoder(reader)
			if err = decoder.Decode(manifest); err != nil {
				return err
			}
			if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
				return Fail("invalid_input", "invalid trailing archive manifest data")
			}
			continue
		}
		if _, exists := entries[header.Name]; exists || !filepath.IsLocal(header.Name) || header.Linkname != "" {
			return Fail("invalid_input", "invalid or repeated legacy knowledge archive entry")
		}
		entry := knowledgeArchiveEntry{Name: header.Name, Size: header.Size}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 || header.Mode != 0700 {
				return Fail("invalid_input", "invalid archive directory header")
			}
			entry.Type = "directory"
		case tar.TypeReg:
			if header.Mode != 0600 {
				return Fail("invalid_input", "invalid archive file mode")
			}
			entry.Type = "file"
			hash := sha256.New()
			n, e := io.Copy(hash, reader)
			if e != nil {
				return e
			}
			if n != header.Size {
				return Fail("invalid_input", "incomplete archive file")
			}
			entry.SHA256 = hex.EncodeToString(hash.Sum(nil))
		default:
			return Fail("invalid_input", "unsupported legacy knowledge archive entry")
		}
		entries[entry.Name] = entry
	}
	if manifest == nil || manifest.Format != "memgov.legacy-knowledge" || manifest.DatabaseSHA256 != databaseSHA || len(manifest.Entries) != len(entries) {
		return Fail("invalid_input", "legacy knowledge archive manifest does not match its database or entries")
	}
	if expected != nil && Digest(manifest) != Digest(expected) {
		return Fail("invalid_input", "legacy knowledge archive manifest differs from the copied sources")
	}
	for _, entry := range manifest.Entries {
		actual, ok := entries[entry.Name]
		if !ok || actual != entry {
			return Fail("invalid_input", "legacy knowledge archive entry checksum or header mismatch")
		}
		delete(entries, entry.Name)
	}
	return nil
}

func writeArchiveMetadata(backup BackupInfo) error {
	destination := backup.Path + ".archive.json"
	temp := destination + ".tmp-" + NewID()
	defer os.Remove(temp)
	f, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = json.NewEncoder(f).Encode(backup); err != nil {
		f.Close()
		return err
	}
	if err = errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	if err = os.Link(temp, destination); err != nil {
		return err
	}
	return syncDir(filepath.Dir(destination))
}

func verifyArchiveMetadata(backup BackupInfo) error {
	f, err := os.OpenFile(backup.Path+".archive.json", os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return Fail("invalid_input", "invalid legacy archive metadata file")
	}
	var recorded BackupInfo
	if err = json.NewDecoder(f).Decode(&recorded); err != nil {
		return err
	}
	if recorded.SHA256 != backup.SHA256 || recorded.Bytes != backup.Bytes || recorded.SchemaVersion != backup.SchemaVersion || recorded.KnowledgeSHA256 != backup.KnowledgeSHA256 {
		return Fail("invalid_input", "legacy archive checksums differ from published metadata")
	}
	return nil
}
