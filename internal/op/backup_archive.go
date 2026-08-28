package op

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/bestruirui/octopus/internal/body"
	"github.com/bestruirui/octopus/internal/model"
)

const relayBodyArchivePrefix = "relay-bodies/"

// DBExportArchive writes the logical database dump and, when requested, the
// compressed external relay body artifacts into one ZIP stream. The database
// dump keeps the original body references, so restoring this archive does not
// require rewriting the log rows.
func DBExportArchive(ctx context.Context, includeLogs, includeStats, includeBodyFiles bool, destination io.Writer) error {
	dump, err := DBExportAll(ctx, includeLogs, includeStats)
	if err != nil {
		return err
	}
	var refs map[string]struct{}
	if includeLogs && includeBodyFiles {
		config := relayBodyStorageConfig()
		refs, err = relayBodyRefsForDump(config, dump.RelayLogs)
		if err != nil {
			return err
		}
		for ref := range refs {
			stored, openErr := body.OpenStored(config, ref)
			if openErr != nil {
				return fmt.Errorf("open relay body %q for export: %w", ref, openErr)
			}
			if closeErr := stored.Close(); closeErr != nil {
				return fmt.Errorf("close relay body %q during export: %w", ref, closeErr)
			}
		}
	}

	archive := zip.NewWriter(destination)
	closed := false
	defer func() {
		if !closed {
			_ = archive.Close()
		}
	}()

	dumpFile, err := archive.Create("database.json")
	if err != nil {
		return fmt.Errorf("create database dump entry: %w", err)
	}
	if err := json.NewEncoder(dumpFile).Encode(dump); err != nil {
		return fmt.Errorf("write database dump entry: %w", err)
	}

	if includeLogs && includeBodyFiles {
		config := relayBodyStorageConfig()
		for ref := range refs {
			if err := ctx.Err(); err != nil {
				return err
			}
			stored, err := body.OpenStored(config, ref)
			if err != nil {
				return fmt.Errorf("open relay body %q for export: %w", ref, err)
			}
			entry, err := archive.Create(relayBodyArchivePrefix + ref)
			if err != nil {
				_ = stored.Close()
				return fmt.Errorf("create relay body archive entry %q: %w", ref, err)
			}
			_, copyErr := io.Copy(entry, stored)
			closeErr := stored.Close()
			if copyErr != nil {
				return fmt.Errorf("copy relay body %q into export: %w", ref, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close relay body %q during export: %w", ref, closeErr)
			}
		}
	}

	if err := archive.Close(); err != nil {
		return fmt.Errorf("close database export archive: %w", err)
	}
	closed = true
	return nil
}

// DBImportArchive restores a ZIP created by DBExportArchive. It validates all
// body references before importing rows and installs body artifacts
// atomically, while allowing an already-restored artifact to be reused.
func DBImportArchive(ctx context.Context, source io.ReaderAt, size int64) (*model.DBImportResult, error) {
	if source == nil || size <= 0 {
		return nil, fmt.Errorf("empty database archive")
	}
	archive, err := zip.NewReader(source, size)
	if err != nil {
		return nil, fmt.Errorf("read database archive: %w", err)
	}

	var dump model.DBDump
	var dumpFile *zip.File
	for _, file := range archive.File {
		if file.Name == "database.json" {
			dumpFile = file
			break
		}
	}
	if dumpFile == nil {
		return nil, fmt.Errorf("database archive is missing database.json")
	}
	reader, err := dumpFile.Open()
	if err != nil {
		return nil, fmt.Errorf("open database dump entry: %w", err)
	}
	decodeErr := json.NewDecoder(reader).Decode(&dump)
	closeErr := reader.Close()
	if decodeErr != nil {
		return nil, fmt.Errorf("decode database dump entry: %w", decodeErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close database dump entry: %w", closeErr)
	}
	if dump.Version != 0 && dump.Version != dbDumpVersion {
		return nil, fmt.Errorf("unsupported dump version: %d", dump.Version)
	}

	config := relayBodyStorageConfig()
	var logs []model.RelayLog
	if dump.IncludeLogs {
		logs = dump.RelayLogs
	}
	requiredRefs, err := relayBodyRefsForDump(config, logs)
	if err != nil {
		return nil, err
	}
	archivedRefs := make(map[string]*zip.File, len(requiredRefs))
	for _, file := range archive.File {
		if file.Name == "database.json" || file.FileInfo().IsDir() {
			continue
		}
		if !strings.HasPrefix(file.Name, relayBodyArchivePrefix) {
			return nil, fmt.Errorf("unsupported database archive entry %q", file.Name)
		}
		ref, err := canonicalRelayBodyRef(strings.TrimPrefix(file.Name, relayBodyArchivePrefix))
		if err != nil {
			return nil, fmt.Errorf("invalid relay body archive entry %q: %w", file.Name, err)
		}
		if _, ok := requiredRefs[ref]; !ok {
			return nil, fmt.Errorf("relay body archive entry %q is not referenced by database dump", file.Name)
		}
		if _, exists := archivedRefs[ref]; exists {
			return nil, fmt.Errorf("duplicate relay body archive entry %q", file.Name)
		}
		archivedRefs[ref] = file
	}

	for ref := range requiredRefs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if exists, existsErr := body.StoredExists(config, ref); existsErr != nil {
			return nil, fmt.Errorf("check relay body %q before import: %w", ref, existsErr)
		} else if exists {
			continue
		}
		file, ok := archivedRefs[ref]
		if !ok {
			return nil, fmt.Errorf("database archive is missing relay body %q", ref)
		}
		bodyReader, openErr := file.Open()
		if openErr != nil {
			return nil, fmt.Errorf("open relay body archive entry %q: %w", ref, openErr)
		}
		installErr := body.InstallStored(config, ref, bodyReader)
		closeErr := bodyReader.Close()
		if installErr != nil {
			return nil, fmt.Errorf("install relay body %q: %w", ref, installErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close relay body archive entry %q: %w", ref, closeErr)
		}
	}

	return DBImportIncremental(ctx, &dump)
}

func relayBodyRefsForDump(config body.Config, logs []model.RelayLog) (map[string]struct{}, error) {
	refs := make(map[string]struct{})
	for _, relayLog := range logs {
		for _, ref := range []string{relayLog.RequestBodyRef, relayLog.ResponseBodyRef} {
			if strings.TrimSpace(ref) == "" {
				continue
			}
			canonical, err := canonicalRelayBodyRef(ref)
			if err != nil {
				return nil, fmt.Errorf("invalid relay body reference %q: %w", ref, err)
			}
			if _, err := body.OpenStored(config, canonical); err != nil {
				// During import, the target file may not exist yet. The same call
				// here is only for reference/path validation, so ignore not-found.
				if !isNotExistError(err) {
					return nil, fmt.Errorf("validate relay body reference %q: %w", ref, err)
				}
			}
			refs[canonical] = struct{}{}
		}
	}
	return refs, nil
}

func canonicalRelayBodyRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("empty relay body reference")
	}
	if filepath.IsAbs(ref) {
		return "", fmt.Errorf("absolute relay body reference is not allowed")
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(ref)))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("relay body reference escapes storage directory")
	}
	return clean, nil
}

func isNotExistError(err error) bool {
	return err != nil && errors.Is(err, fs.ErrNotExist)
}
