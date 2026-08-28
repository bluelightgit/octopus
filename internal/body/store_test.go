package body

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCaptureKeepsSmallBodyInline(t *testing.T) {
	config := Config{
		Enabled:         true,
		Directory:       t.TempDir(),
		InlineMaxBytes:  64,
		PreviewMaxBytes: 16,
		Compression:     CompressionGzip,
	}

	input := []byte(`{"message":"small body"}`)
	capture := NewCapture(config)
	if err := capture.Write(input); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	artifact, err := capture.Finish()
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	if artifact.Ref != "" {
		t.Fatalf("small body unexpectedly has external ref %q", artifact.Ref)
	}
	if artifact.Truncated {
		t.Fatal("small body unexpectedly marked truncated")
	}
	if !bytes.Equal(artifact.Inline, input) {
		t.Fatalf("inline body mismatch: got %q want %q", artifact.Inline, input)
	}
	if artifact.Size != int64(len(input)) {
		t.Fatalf("size mismatch: got %d want %d", artifact.Size, len(input))
	}
	if artifact.SHA256 == "" {
		t.Fatal("expected sha256 metadata")
	}
}

func TestCaptureStreamsLargeBodyAndOpenReturnsExactBytes(t *testing.T) {
	config := Config{
		Enabled:         true,
		Directory:       t.TempDir(),
		InlineMaxBytes:  8,
		PreviewMaxBytes: 5,
		Compression:     CompressionGzip,
	}
	input := []byte("0123456789abcdefghijklmnopqrstuvwxyz")

	capture := NewCapture(config)
	for _, chunk := range [][]byte{input[:3], input[3:11], input[11:]} {
		if err := capture.Write(chunk); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}
	artifact, err := capture.Finish()
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}

	if artifact.Ref == "" {
		t.Fatal("large body did not get an external ref")
	}
	if !artifact.Truncated {
		t.Fatal("large body was not marked truncated")
	}
	if !bytes.Equal(artifact.Inline, input[:config.PreviewMaxBytes]) {
		t.Fatalf("preview mismatch: got %q want %q", artifact.Inline, input[:config.PreviewMaxBytes])
	}
	if artifact.Size != int64(len(input)) {
		t.Fatalf("size mismatch: got %d want %d", artifact.Size, len(input))
	}

	storedPath := filepath.Join(config.Directory, filepath.FromSlash(artifact.Ref))
	if _, err := os.Stat(storedPath); err != nil {
		t.Fatalf("stored body is missing: %v", err)
	}
	reader, err := Open(config, artifact.Ref)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		t.Fatalf("reading stored body failed: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("closing stored body failed: %v", closeErr)
	}
	if !bytes.Equal(got, input) {
		t.Fatalf("stored body mismatch: got %q want %q", got, input)
	}
}

func TestCaptureKeepsRawBinaryPrefixInline(t *testing.T) {
	config := Config{
		Enabled:         true,
		Directory:       t.TempDir(),
		InlineMaxBytes:  64,
		PreviewMaxBytes: 64,
		Compression:     CompressionNone,
	}
	input := []byte{0x00, 0xff, 0x01, 0xfe}

	capture := NewCapture(config)
	_ = capture.Write(input)
	artifact, err := capture.Finish()
	if err != nil {
		t.Fatalf("Finish failed: %v", err)
	}
	content, encoding := EncodeInline(artifact.Inline)
	decoded, err := DecodeInline(content, encoding)
	if err != nil {
		t.Fatalf("DecodeInline failed: %v", err)
	}
	if !bytes.Equal(decoded, input) {
		t.Fatalf("decoded binary body mismatch: got %v want %v", decoded, input)
	}
}

func TestSweepRemovesOnlyUnreferencedBodiesAndOldTemps(t *testing.T) {
	directory := t.TempDir()
	config := Config{Enabled: true, Directory: directory}
	dateDir := filepath.Join(directory, "20260101")
	if err := os.MkdirAll(dateDir, 0750); err != nil {
		t.Fatalf("create date directory failed: %v", err)
	}
	for name, content := range map[string]string{
		"kept.body":   "kept",
		"orphan.body": "orphan",
		"orphan.gz":   "orphan",
		"ignored.txt": "ignored",
	} {
		if err := os.WriteFile(filepath.Join(dateDir, name), []byte(content), 0600); err != nil {
			t.Fatalf("create %s failed: %v", name, err)
		}
	}
	tempPath := filepath.Join(directory, ".octopus-body-old.tmp")
	if err := os.WriteFile(tempPath, []byte("temp"), 0600); err != nil {
		t.Fatalf("create temp failed: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(tempPath, old, old); err != nil {
		t.Fatalf("age temp failed: %v", err)
	}

	removed, err := Sweep(config, map[string]struct{}{"20260101/kept.body": {}}, time.Minute)
	if err != nil {
		t.Fatalf("Sweep failed: %v", err)
	}
	if removed != 3 {
		t.Fatalf("removed count mismatch: got %d want 3", removed)
	}
	if _, err := os.Stat(filepath.Join(dateDir, "kept.body")); err != nil {
		t.Fatalf("referenced body was removed: %v", err)
	}
	for _, name := range []string{"orphan.body", "orphan.gz"} {
		if _, err := os.Stat(filepath.Join(dateDir, name)); !os.IsNotExist(err) {
			t.Fatalf("orphan %s still exists, err=%v", name, err)
		}
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("old temp still exists, err=%v", err)
	}
}

func TestResolveRefRejectsPathTraversal(t *testing.T) {
	config := Config{Directory: t.TempDir()}
	for _, ref := range []string{"../outside.body", "/tmp/outside.body", "a/../../outside.body"} {
		if _, err := Open(config, ref); err == nil {
			t.Fatalf("Open accepted unsafe ref %q", ref)
		}
	}
}
