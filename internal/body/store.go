package body

import (
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	EncodingUTF8   = "utf8"
	EncodingBase64 = "base64"

	CompressionGzip = "gzip"
	CompressionNone = "none"
)

// Config controls how relay request/response bodies are retained. Inline is
// deliberately bounded; complete bodies larger than that limit are streamed
// to the external store by Capture.
type Config struct {
	Enabled         bool
	Directory       string
	InlineMaxBytes  int64
	PreviewMaxBytes int64
	Compression     string
}

func (c Config) WithDefaults() Config {
	if c.Directory == "" {
		c.Directory = "data/relay-bodies"
	}
	if c.InlineMaxBytes <= 0 {
		c.InlineMaxBytes = 1 << 20
	}
	if c.PreviewMaxBytes <= 0 {
		c.PreviewMaxBytes = 256 << 10
	}
	if c.PreviewMaxBytes > c.InlineMaxBytes {
		c.PreviewMaxBytes = c.InlineMaxBytes
	}
	switch strings.ToLower(strings.TrimSpace(c.Compression)) {
	case "", CompressionGzip:
		c.Compression = CompressionGzip
	case CompressionNone:
		c.Compression = CompressionNone
	default:
		c.Compression = CompressionGzip
	}
	return c
}

// Artifact is the durable representation of one captured body. Inline holds
// either the complete body or its exact prefix. Ref is empty when the body is
// fully inline or external storage failed.
type Artifact struct {
	Inline       []byte
	Ref          string
	Size         int64
	SHA256       string
	Encoding     string
	Truncated    bool
	StorageError string
}

// Capture retains only a bounded in-memory prefix. Once the body crosses the
// inline limit, it writes the complete raw bytes to a temporary external file.
type Capture struct {
	config Config

	inline  []byte
	preview []byte
	total   int64
	hash    hash.Hash

	external   bool
	tempPath   string
	tempFile   *os.File
	compressor io.WriteCloser
	writer     io.Writer
	storageErr error
	finished   bool
}

func NewCapture(config Config) *Capture {
	config = config.WithDefaults()
	return &Capture{
		config:  config,
		hash:    sha256.New(),
		inline:  make([]byte, 0, minInt64(config.InlineMaxBytes, 64<<10)),
		preview: make([]byte, 0, minInt64(config.PreviewMaxBytes, 64<<10)),
	}
}

// Write adds raw bytes to the capture. If external storage cannot be opened
// or written, the error is retained for Finish while the caller can continue
// serving the request normally.
func (c *Capture) Write(p []byte) error {
	if c == nil || len(p) == 0 {
		return nil
	}
	if c.finished {
		return errors.New("body capture is already finished")
	}

	previousTotal := c.total
	c.total += int64(len(p))
	_, _ = c.hash.Write(p)
	c.preview = appendPrefix(c.preview, p, c.config.PreviewMaxBytes)

	if c.external {
		return c.writeExternal(p)
	}

	inlineLimit := c.config.InlineMaxBytes
	if previousTotal < inlineLimit && c.total <= inlineLimit {
		c.inline = append(c.inline, p...)
		return nil
	}

	keep := inlineLimit - previousTotal
	if keep < 0 {
		keep = 0
	}
	if keep > int64(len(p)) {
		keep = int64(len(p))
	}
	if keep > 0 {
		c.inline = append(c.inline, p[:keep]...)
	}

	if !c.config.Enabled || c.storageErr != nil {
		return c.storageErr
	}

	if err := c.startExternal(); err != nil {
		c.storageErr = err
		c.inline = cloneBytes(c.preview)
		return err
	}
	if err := c.writeExternal(c.inline); err != nil {
		c.storageErr = err
		c.abortExternal()
		c.external = false
		c.inline = cloneBytes(c.preview)
		return err
	}
	c.inline = nil
	if err := c.writeExternal(p[keep:]); err != nil {
		c.storageErr = err
		c.abortExternal()
		c.external = false
		c.inline = cloneBytes(c.preview)
		return err
	}
	return nil
}

// Finish closes and atomically publishes an external body, or returns the
// bounded inline representation when external storage is disabled/unavailable.
func (c *Capture) Finish() (Artifact, error) {
	if c == nil {
		return Artifact{}, nil
	}
	if c.finished {
		return Artifact{}, errors.New("body capture is already finished")
	}
	c.finished = true

	artifact := Artifact{
		Size:      c.total,
		SHA256:    hex.EncodeToString(c.hash.Sum(nil)),
		Truncated: c.total > int64(len(c.inline)),
	}

	if c.total == 0 {
		return artifact, nil
	}

	if c.external && c.storageErr == nil {
		if err := c.closeExternal(); err != nil {
			c.storageErr = err
			c.abortExternal()
			c.inline = cloneBytes(c.preview)
		} else if ref, err := publishExternal(c.config, c.tempPath); err != nil {
			c.storageErr = err
			c.tempPath = ""
			c.inline = cloneBytes(c.preview)
		} else {
			c.tempPath = ""
			artifact.Ref = ref
			artifact.Inline = cloneBytes(c.preview)
			artifact.Truncated = true
			artifact.Encoding = EncodingForBytes(artifact.Inline)
			return artifact, nil
		}
	}

	artifact.Inline = cloneBytes(c.inline)
	artifact.Truncated = c.total > int64(len(artifact.Inline))
	artifact.Encoding = EncodingForBytes(artifact.Inline)
	if c.storageErr != nil {
		artifact.StorageError = "storage_failed"
	}
	return artifact, c.storageErr
}

// Discard closes and removes an unpublished temporary body. It is used when a
// capture is replaced before the relay log is saved.
func (c *Capture) Discard() {
	if c == nil || c.finished {
		return
	}
	c.finished = true
	c.abortExternal()
}

func (c *Capture) startExternal() error {
	if err := os.MkdirAll(c.config.Directory, 0750); err != nil {
		return err
	}
	file, err := os.CreateTemp(c.config.Directory, ".octopus-body-*.tmp")
	if err != nil {
		return err
	}
	c.tempFile = file
	c.tempPath = file.Name()
	c.writer = file
	if c.config.Compression == CompressionGzip {
		c.compressor = gzip.NewWriter(file)
		c.writer = c.compressor
	}
	c.external = true
	return nil
}

func (c *Capture) writeExternal(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	if c.writer == nil {
		return errors.New("body external writer is not initialized")
	}
	n, err := c.writer.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

func (c *Capture) closeExternal() error {
	var firstErr error
	if c.compressor != nil {
		if err := c.compressor.Close(); err != nil {
			firstErr = err
		}
	}
	if c.tempFile != nil {
		if err := c.tempFile.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (c *Capture) abortExternal() {
	if c.compressor != nil {
		_ = c.compressor.Close()
	}
	if c.tempFile != nil {
		_ = c.tempFile.Close()
	}
	if c.tempPath != "" {
		_ = os.Remove(c.tempPath)
	}
	c.external = false
	c.tempPath = ""
	c.tempFile = nil
	c.compressor = nil
	c.writer = nil
}

func publishExternal(config Config, tempPath string) (string, error) {
	if tempPath == "" {
		return "", errors.New("empty body temporary path")
	}
	config = config.WithDefaults()
	dateDir := time.Now().UTC().Format("20060102")
	finalDir := filepath.Join(config.Directory, dateDir)
	if err := os.MkdirAll(finalDir, 0750); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}

	name, err := randomName()
	if err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	extension := ".body"
	if config.Compression == CompressionGzip {
		extension = ".gz"
	}
	finalName := name + extension
	finalPath := filepath.Join(finalDir, finalName)
	if err := os.Rename(tempPath, finalPath); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	return filepath.ToSlash(filepath.Join(dateDir, finalName)), nil
}

func randomName() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func EncodingForBytes(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	if utf8.Valid(data) {
		return EncodingUTF8
	}
	return EncodingBase64
}

// EncodeInline converts a bounded inline body into the historical log JSON
// representation while retaining enough metadata to decode it exactly.
func EncodeInline(data []byte) (string, string) {
	encoding := EncodingForBytes(data)
	if encoding == EncodingBase64 {
		encoded, err := json.Marshal(map[string]string{
			"base64": base64.StdEncoding.EncodeToString(data),
		})
		if err == nil {
			return string(encoded), encoding
		}
	}
	return string(data), encoding
}

// DecodeInline reverses EncodeInline for the download endpoint. An empty
// encoding is treated as UTF-8 for compatibility with legacy log rows.
func DecodeInline(content, encoding string) ([]byte, error) {
	if encoding != EncodingBase64 {
		return []byte(content), nil
	}
	var payload struct {
		Base64 string `json:"base64"`
	}
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(payload.Base64)
}

func appendPrefix(dst, src []byte, limit int64) []byte {
	if limit <= int64(len(dst)) || len(src) == 0 {
		return dst
	}
	remain := limit - int64(len(dst))
	if remain > int64(len(src)) {
		remain = int64(len(src))
	}
	return append(dst, src[:remain]...)
}

func cloneBytes(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	clone := make([]byte, len(data))
	copy(clone, data)
	return clone
}

func minInt64(a, b int64) int {
	if a < b {
		return int(a)
	}
	return int(b)
}

// Open opens and transparently decompresses a stored body. Ref is always
// validated against the configured root to prevent path traversal.
func Open(config Config, ref string) (io.ReadCloser, error) {
	path, err := resolveRef(config, ref)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(strings.ToLower(path), ".gz") {
		reader, err := gzip.NewReader(file)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		return &compoundReadCloser{Reader: reader, closers: []io.Closer{reader, file}}, nil
	}
	return file, nil
}

// OpenStored opens the compressed/on-disk representation without decoding it.
// It is used by the backup archive so a restore keeps the original reference
// and does not need to recompress a potentially large body.
func OpenStored(config Config, ref string) (io.ReadCloser, error) {
	path, err := resolveRef(config, ref)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

func StoredExists(config Config, ref string) (bool, error) {
	path, err := resolveRef(config, ref)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !info.IsDir(), nil
}

// InstallStored atomically installs an archived on-disk body. Existing files
// are kept so importing the same archive twice is idempotent and cannot
// overwrite an already referenced artifact.
func InstallStored(config Config, ref string, source io.Reader) error {
	path, err := resolveRef(config, ref)
	if err != nil {
		return err
	}
	if exists, existsErr := StoredExists(config, ref); existsErr != nil {
		return existsErr
	} else if exists {
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".octopus-body-import-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}
	if _, err := io.Copy(temp, source); err != nil {
		cleanup()
		return err
	}
	if err := temp.Chmod(0600); err != nil {
		cleanup()
		return err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Link(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		if exists, existsErr := StoredExists(config, ref); existsErr == nil && exists {
			return nil
		}
		return err
	}
	_ = os.Remove(tempPath)
	return nil
}

func Delete(config Config, ref string) error {
	path, err := resolveRef(config, ref)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Sweep removes external files that are not referenced by the supplied set.
// It also removes stale temporary captures. This is intended for startup or a
// low-frequency maintenance pass, not every request.
func Sweep(config Config, refs map[string]struct{}, tempAge time.Duration) (int, error) {
	config = config.WithDefaults()
	root, err := filepath.Abs(config.Directory)
	if err != nil {
		return 0, err
	}
	removed := 0
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}

		name := entry.Name()
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		isTemp := strings.HasPrefix(name, ".octopus-body-") && strings.HasSuffix(name, ".tmp")
		isBody := strings.HasSuffix(strings.ToLower(name), ".gz") || strings.HasSuffix(strings.ToLower(name), ".body")
		if !isTemp && !isBody {
			return nil
		}
		if isTemp {
			if tempAge <= 0 || time.Since(info.ModTime()) < tempAge {
				return nil
			}
		} else {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			if _, ok := refs[filepath.ToSlash(rel)]; ok {
				return nil
			}
		}
		if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
		removed++
		return nil
	})
	return removed, err
}

func resolveRef(config Config, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", errors.New("empty body reference")
	}
	config = config.WithDefaults()
	root, err := filepath.Abs(config.Directory)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(ref) {
		return "", errors.New("absolute body reference is not allowed")
	}
	path := filepath.Clean(filepath.Join(root, filepath.FromSlash(ref)))
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", errors.New("body reference escapes storage directory")
	}
	if err := rejectSymlinkComponents(root, rel); err != nil {
		return "", err
	}
	return path, nil
}

func rejectSymlinkComponents(root, rel string) error {
	if rel == "." || rel == "" {
		return nil
	}
	current := root
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("body reference contains a symlink")
		}
	}
	return nil
}

type compoundReadCloser struct {
	io.Reader
	closers []io.Closer
}

func (c *compoundReadCloser) Close() error {
	var firstErr error
	for _, closer := range c.closers {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
