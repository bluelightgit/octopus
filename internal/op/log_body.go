package op

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/body"
	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"gorm.io/gorm"
)

var (
	ErrRelayLogNotFound     = errors.New("relay log not found")
	ErrRelayLogBodyNotFound = errors.New("relay log body not found")
	ErrRelayLogBodyKind     = errors.New("relay log body kind must be request or response")
)

type relayLogBodyRefs struct {
	ID              int64  `gorm:"column:id"`
	RequestBodyRef  string `gorm:"column:request_body_ref"`
	ResponseBodyRef string `gorm:"column:response_body_ref"`
}

func relayBodyStorageConfig() body.Config {
	config := conf.AppConfig.RelayBodyStorage.WithDefaults()
	return body.Config{
		Enabled:         config.Enabled,
		Directory:       config.Directory,
		InlineMaxBytes:  config.InlineMaxBytes,
		PreviewMaxBytes: config.PreviewMaxBytes,
		Compression:     config.Compression,
	}
}

// RelayLogGet returns a log from the in-memory tail first, then the database.
// Logs can remain in the tail until the periodic flush, so body downloads must
// work before the row is durable in SQLite as well.
func RelayLogGet(ctx context.Context, id int64) (*model.RelayLog, error) {
	if id <= 0 {
		return nil, ErrRelayLogNotFound
	}

	relayLogCacheLock.Lock()
	for i := len(relayLogCache) - 1; i >= 0; i-- {
		if relayLogCache[i].ID == id {
			log := relayLogCache[i]
			relayLogCacheLock.Unlock()
			return &log, nil
		}
	}
	relayLogCacheLock.Unlock()

	var relayLog model.RelayLog
	result := db.GetReadDB().WithContext(ctx).Where("id = ?", id).First(&relayLog)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return nil, ErrRelayLogNotFound
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return &relayLog, nil
}

// RelayLogBodyOpen opens the exact original request or client-visible
// response body. The returned reader is bounded by the configured storage
// file, not by the preview kept in SQLite.
func RelayLogBodyOpen(ctx context.Context, id int64, kind string) (io.ReadCloser, *model.RelayLog, error) {
	relayLog, err := RelayLogGet(ctx, id)
	if err != nil {
		return nil, nil, err
	}

	content, encoding, ref, size, err := relayLogBodyFields(relayLog, kind)
	if err != nil {
		return nil, relayLog, err
	}
	if ref != "" {
		reader, openErr := body.Open(relayBodyStorageConfig(), ref)
		if openErr != nil {
			return nil, relayLog, fmt.Errorf("%w: %v", ErrRelayLogBodyNotFound, openErr)
		}
		return reader, relayLog, nil
	}

	if content == "" {
		if size == 0 {
			return io.NopCloser(bytes.NewReader(nil)), relayLog, nil
		}
		return nil, relayLog, ErrRelayLogBodyNotFound
	}
	decoded, decodeErr := body.DecodeInline(content, encoding)
	if decodeErr != nil {
		return nil, relayLog, fmt.Errorf("decode inline relay body: %w", decodeErr)
	}
	return io.NopCloser(bytes.NewReader(decoded)), relayLog, nil
}

func relayLogBodyFields(relayLog *model.RelayLog, kind string) (content, encoding, ref string, size int64, err error) {
	if relayLog == nil {
		return "", "", "", 0, ErrRelayLogNotFound
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "request":
		return relayLog.RequestContent, relayLog.RequestBodyEncoding, relayLog.RequestBodyRef, relayLog.RequestBodySize, nil
	case "response":
		return relayLog.ResponseContent, relayLog.ResponseBodyEncoding, relayLog.ResponseBodyRef, relayLog.ResponseBodySize, nil
	default:
		return "", "", "", 0, ErrRelayLogBodyKind
	}
}

// RelayLogBodySweep removes body files left behind by an interrupted request,
// failed database flush, or a manual database import. The reference set also
// includes the in-memory tail so a just-finished request is never removed
// before its cache row is flushed.
func RelayLogBodySweep(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	refs := make(map[string]struct{})
	relayLogCacheLock.Lock()
	for _, relayLog := range relayLogCache {
		addRelayBodyRef(refs, relayLog.RequestBodyRef)
		addRelayBodyRef(refs, relayLog.ResponseBodyRef)
	}
	relayLogCacheLock.Unlock()

	var rows []relayLogBodyRefs
	result := db.GetReadDB().WithContext(ctx).
		Model(&model.RelayLog{}).
		Select("request_body_ref, response_body_ref").
		Find(&rows)
	if result.Error != nil {
		return 0, result.Error
	}
	for _, row := range rows {
		addRelayBodyRef(refs, row.RequestBodyRef)
		addRelayBodyRef(refs, row.ResponseBodyRef)
	}

	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return body.Sweep(relayBodyStorageConfig(), refs, 24*time.Hour)
}

func addRelayBodyRef(refs map[string]struct{}, ref string) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return
	}
	refs[filepath.ToSlash(filepath.Clean(filepath.FromSlash(ref)))] = struct{}{}
}
