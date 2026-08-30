package storage

import (
	"context"
	"io"
	"time"
)

// Object describes a single stored backup.
type Object struct {
	Name    string
	Size    int64
	ModTime time.Time
}

// Provider is a destination for backup files.
//
// Store consumes data until EOF. If data returns an error mid-stream the
// implementation must discard the partial object and return that error, so a
// failed dump never leaves a plausible-looking backup behind.
type Provider interface {
	Store(ctx context.Context, filename string, data io.Reader) error
	List(ctx context.Context) ([]Object, error)
	Delete(ctx context.Context, filename string) error
}
