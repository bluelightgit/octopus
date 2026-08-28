package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/bestruirui/octopus/internal/body"
	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/gin-gonic/gin"
)

func TestDownloadLogBodyStreamsExternalBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	databasePath := filepath.Join(t.TempDir(), "handler.db")
	if err := db.InitDB("sqlite", databasePath, false); err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	previous := conf.AppConfig.RelayBodyStorage
	t.Cleanup(func() { conf.AppConfig.RelayBodyStorage = previous })
	storageDir := filepath.Join(t.TempDir(), "relay-bodies")
	conf.AppConfig.RelayBodyStorage = conf.RelayBodyStorage{
		Enabled:         true,
		Directory:       storageDir,
		InlineMaxBytes:  4,
		PreviewMaxBytes: 2,
		Compression:     "gzip",
	}

	original := []byte("download me exactly")
	capture := body.NewCapture(body.Config{
		Enabled:         true,
		Directory:       storageDir,
		InlineMaxBytes:  4,
		PreviewMaxBytes: 2,
		Compression:     body.CompressionGzip,
	})
	if err := capture.Write(original); err != nil {
		t.Fatalf("capture Write failed: %v", err)
	}
	artifact, err := capture.Finish()
	if err != nil {
		t.Fatalf("capture Finish failed: %v", err)
	}
	preview, encoding := body.EncodeInline(artifact.Inline)
	if err := db.GetDB().Create(&model.RelayLog{
		ID:                      9002,
		RequestContent:          preview,
		RequestContentTruncated: true,
		RequestBodyRef:          artifact.Ref,
		RequestBodySize:         artifact.Size,
		RequestBodySHA256:       artifact.SHA256,
		RequestBodyEncoding:     encoding,
	}).Error; err != nil {
		t.Fatalf("create relay log failed: %v", err)
	}

	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/log/9002/body?kind=request", nil)
	context.Params = gin.Params{{Key: "id", Value: "9002"}}

	downloadLogBody(context)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !bytes.Equal(recorder.Body.Bytes(), original) {
		t.Fatalf("downloaded body mismatch: got %q want %q", recorder.Body.Bytes(), original)
	}
	if recorder.Header().Get("X-Content-SHA256") != artifact.SHA256 {
		t.Fatalf("unexpected body hash header: %q", recorder.Header().Get("X-Content-SHA256"))
	}
}
