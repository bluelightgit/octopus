package relay

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"

	"github.com/bluelightgit/octopus/internal/body"
	"github.com/bluelightgit/octopus/internal/conf"
)

func TestRelayBodyCaptureUsesBoundedPreviewAndExternalOriginal(t *testing.T) {
	previous := conf.AppConfig.RelayBodyStorage
	t.Cleanup(func() { conf.AppConfig.RelayBodyStorage = previous })

	directory := t.TempDir()
	conf.AppConfig.RelayBodyStorage = conf.RelayBodyStorage{
		Enabled:         true,
		Directory:       filepath.Join(directory, "relay-bodies"),
		InlineMaxBytes:  1024,
		PreviewMaxBytes: 128,
		Compression:     "gzip",
	}

	input := bytes.Repeat([]byte("octopus-body-"), 10000)
	capture := newRelayBodyCapture(16)
	for start := 0; start < len(input); {
		end := start + 37
		if end > len(input) {
			end = len(input)
		}
		if err := capture.Write(input[start:end]); err != nil {
			t.Fatalf("capture Write failed: %v", err)
		}
		start = end
	}
	artifact, err := capture.Finish()
	if err != nil {
		t.Fatalf("capture Finish failed: %v", err)
	}
	if artifact.Ref == "" || !artifact.Truncated {
		t.Fatalf("expected external truncated artifact: %+v", artifact)
	}
	if !bytes.Equal(artifact.Inline, input[:conf.AppConfig.RelayBodyStorage.PreviewMaxBytes]) {
		t.Fatalf("preview is not the exact prefix")
	}

	reader, err := body.Open(body.Config{Directory: conf.AppConfig.RelayBodyStorage.Directory}, artifact.Ref)
	if err != nil {
		t.Fatalf("open external body failed: %v", err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		t.Fatalf("read external body failed: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close external body failed: %v", closeErr)
	}
	if !bytes.Equal(got, input) {
		t.Fatalf("external body is not exact: got %d bytes want %d", len(got), len(input))
	}
}

func TestRelayBodyCaptureCompatibilityLimitWhenExternalStorageDisabled(t *testing.T) {
	previous := conf.AppConfig.RelayBodyStorage
	t.Cleanup(func() { conf.AppConfig.RelayBodyStorage = previous })
	conf.AppConfig.RelayBodyStorage = conf.RelayBodyStorage{Enabled: false}

	input := bytes.Repeat([]byte("legacy-"), 100)
	capture := newRelayBodyCapture(32)
	_ = capture.Write(input)
	artifact, err := capture.Finish()
	if err != nil {
		t.Fatalf("capture Finish failed: %v", err)
	}
	if artifact.Ref != "" || len(artifact.Inline) != 32 || !artifact.Truncated {
		t.Fatalf("legacy limit was not applied: %+v", artifact)
	}
}
