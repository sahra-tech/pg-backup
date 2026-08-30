package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type Local struct {
	basePath string
}

func NewLocal(basePath string) *Local {
	return &Local{basePath: basePath}
}

// Store writes to a temporary file and renames it into place only after the
// stream completes, so an interrupted or failed dump never leaves a partial
// file that looks like a finished backup.
func (l *Local) Store(ctx context.Context, filename string, data io.Reader) error {
	if err := os.MkdirAll(l.basePath, 0o755); err != nil {
		return err
	}

	finalPath := filepath.Join(l.basePath, filename)
	tmp, err := os.CreateTemp(l.basePath, filename+".part-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()

	// Any failure past this point must leave nothing behind.
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmp, data); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return err
	}
	committed = true
	return nil
}

func (l *Local) List(_ context.Context) ([]Object, error) {
	entries, err := os.ReadDir(l.basePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var objects []Object
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			// The file vanished between ReadDir and Info; nothing to prune.
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		objects = append(objects, Object{
			Name:    entry.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	return objects, nil
}

func (l *Local) Delete(_ context.Context, filename string) error {
	// Refuse anything that would escape the backup directory.
	if filepath.Base(filename) != filename {
		return fmt.Errorf("refusing to delete %q: not a plain filename", filename)
	}
	err := os.Remove(filepath.Join(l.basePath, filename))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// interface guard
var _ Provider = (*Local)(nil)
