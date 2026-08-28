package op

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/body"
	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
)

func TestDBExportArchiveAndImportRestoresExternalBody(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "archive.db")
	if err := db.InitDB("sqlite", databasePath, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	sourceDir := filepath.Join(t.TempDir(), "source-bodies")
	config := conf.RelayBodyStorage{
		Enabled:         true,
		Directory:       sourceDir,
		InlineMaxBytes:  8,
		PreviewMaxBytes: 4,
		Compression:     "gzip",
	}
	conf.AppConfig.RelayBodyStorage = config

	original := []byte("the complete body survives the archive")
	capture := body.NewCapture(body.Config{
		Enabled:         true,
		Directory:       sourceDir,
		InlineMaxBytes:  8,
		PreviewMaxBytes: 4,
		Compression:     body.CompressionGzip,
	})
	if err := capture.Write(original); err != nil {
		t.Fatalf("capture Write failed: %v", err)
	}
	artifact, err := capture.Finish()
	if err != nil {
		t.Fatalf("capture Finish failed: %v", err)
	}
	if artifact.Ref == "" {
		t.Fatal("expected external body reference")
	}
	requestContent, requestEncoding := body.EncodeInline(artifact.Inline)
	row := model.RelayLog{
		ID:                      9001,
		RequestContent:          requestContent,
		RequestContentTruncated: true,
		RequestBodyRef:          artifact.Ref,
		RequestBodySize:         artifact.Size,
		RequestBodySHA256:       artifact.SHA256,
		RequestBodyEncoding:     requestEncoding,
	}
	if err := db.GetDB().Create(&row).Error; err != nil {
		t.Fatalf("create relay log failed: %v", err)
	}

	bodyReader, bodyLog, err := RelayLogBodyOpen(context.Background(), row.ID, "request")
	if err != nil {
		t.Fatalf("RelayLogBodyOpen failed: %v", err)
	}
	opened, readErr := io.ReadAll(bodyReader)
	closeErr := bodyReader.Close()
	if readErr != nil || closeErr != nil || bodyLog == nil || !bytes.Equal(opened, original) {
		t.Fatalf("opened body mismatch: read=%v close=%v body=%q", readErr, closeErr, opened)
	}

	orphanDir := filepath.Join(sourceDir, "20260101")
	if err := os.MkdirAll(orphanDir, 0750); err != nil {
		t.Fatalf("create orphan directory failed: %v", err)
	}
	orphanPath := filepath.Join(orphanDir, "orphan.body")
	if err := os.WriteFile(orphanPath, []byte("orphan"), 0600); err != nil {
		t.Fatalf("create orphan body failed: %v", err)
	}
	removed, err := RelayLogBodySweep(context.Background())
	if err != nil {
		t.Fatalf("RelayLogBodySweep failed: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected one orphan body removed, got %d", removed)
	}
	if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
		t.Fatalf("orphan body still exists, err=%v", err)
	}

	var archive bytes.Buffer
	if err := DBExportArchive(context.Background(), true, false, true, &archive); err != nil {
		t.Fatalf("DBExportArchive failed: %v", err)
	}

	restoreDir := filepath.Join(t.TempDir(), "restored-bodies")
	conf.AppConfig.RelayBodyStorage.Directory = restoreDir
	result, err := DBImportArchive(context.Background(), bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatalf("DBImportArchive failed: %v", err)
	}
	if result == nil {
		t.Fatal("DBImportArchive returned nil result")
	}

	reader, err := body.Open(body.Config{Directory: restoreDir}, artifact.Ref)
	if err != nil {
		t.Fatalf("open restored body failed: %v", err)
	}
	restored, readErr := io.ReadAll(reader)
	closeErr = reader.Close()
	if readErr != nil {
		t.Fatalf("read restored body failed: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close restored body failed: %v", closeErr)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("restored body mismatch: got %q want %q", restored, original)
	}

	conf.AppConfig.RelayBodyStorage.Directory = sourceDir
	if err := RelayLogClear(context.Background()); err != nil {
		t.Fatalf("RelayLogClear failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sourceDir, filepath.FromSlash(artifact.Ref))); !os.IsNotExist(err) {
		t.Fatalf("referenced body still exists after clearing logs, err=%v", err)
	}
}
