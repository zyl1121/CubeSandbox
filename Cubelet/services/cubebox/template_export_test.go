// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

func TestExportTemplateRootfsReaderFallsBackToRequestMetadataOnCatalogMiss(t *testing.T) {
	devPath := filepath.Join(t.TempDir(), "rootfs.ext4")
	want := []byte("rootfs-bytes")
	if err := os.WriteFile(devPath, want, 0o644); err != nil {
		t.Fatalf("write temp rootfs file: %v", err)
	}

	origGetLocalSnapshot := getLocalSnapshotForExport
	origResolveCowDevPath := resolveCowDevPathForExport
	t.Cleanup(func() {
		getLocalSnapshotForExport = origGetLocalSnapshot
		resolveCowDevPathForExport = origResolveCowDevPath
	})

	getLocalSnapshotForExport = func(context.Context, string) (*storage.SnapshotCatalogEntry, error) {
		return nil, storage.ErrSnapshotCatalogNotFound
	}
	resolveCowDevPathForExport = func(_ context.Context, rootfsVol, rootfsKind string) (string, error) {
		if rootfsVol != "fallback-vol" || rootfsKind != "snapshot" {
			t.Fatalf("unexpected fallback metadata: vol=%q kind=%q", rootfsVol, rootfsKind)
		}
		return devPath, nil
	}

	reader, sizeBytes, err := exportTemplateRootfsReader(context.Background(), "tpl-1", "fallback-vol", "snapshot", uint64(len(want)))
	if err != nil {
		t.Fatalf("exportTemplateRootfsReader failed: %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read exported content: %v", err)
	}
	if sizeBytes != uint64(len(want)) {
		t.Fatalf("sizeBytes=%d, want %d", sizeBytes, len(want))
	}
	if string(got) != string(want) {
		t.Fatalf("exported content=%q, want %q", string(got), string(want))
	}
}

func TestExportTemplateRootfsReaderPrefersCatalogMetadata(t *testing.T) {
	devPath := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := os.WriteFile(devPath, []byte("catalog-bytes"), 0o644); err != nil {
		t.Fatalf("write temp rootfs file: %v", err)
	}

	origGetLocalSnapshot := getLocalSnapshotForExport
	origResolveCowDevPath := resolveCowDevPathForExport
	t.Cleanup(func() {
		getLocalSnapshotForExport = origGetLocalSnapshot
		resolveCowDevPathForExport = origResolveCowDevPath
	})

	getLocalSnapshotForExport = func(context.Context, string) (*storage.SnapshotCatalogEntry, error) {
		return &storage.SnapshotCatalogEntry{
			RootfsVol:       "catalog-vol",
			RootfsKind:      "volume",
			RootfsSizeBytes: 13,
		}, nil
	}
	resolveCowDevPathForExport = func(_ context.Context, rootfsVol, rootfsKind string) (string, error) {
		if rootfsVol != "catalog-vol" || rootfsKind != "volume" {
			t.Fatalf("catalog metadata should override fallback, got vol=%q kind=%q", rootfsVol, rootfsKind)
		}
		return devPath, nil
	}

	reader, sizeBytes, err := exportTemplateRootfsReader(context.Background(), "tpl-1", "fallback-vol", "snapshot", 99)
	if err != nil {
		t.Fatalf("exportTemplateRootfsReader failed: %v", err)
	}
	reader.Close()
	if sizeBytes != 13 {
		t.Fatalf("sizeBytes=%d, want %d", sizeBytes, 13)
	}
}

func TestExportTemplateRootfsReaderRejectsIncompleteMetadata(t *testing.T) {
	origGetLocalSnapshot := getLocalSnapshotForExport
	origResolveCowDevPath := resolveCowDevPathForExport
	t.Cleanup(func() {
		getLocalSnapshotForExport = origGetLocalSnapshot
		resolveCowDevPathForExport = origResolveCowDevPath
	})

	getLocalSnapshotForExport = func(context.Context, string) (*storage.SnapshotCatalogEntry, error) {
		return nil, storage.ErrSnapshotCatalogNotFound
	}
	resolveCowDevPathForExport = func(context.Context, string, string) (string, error) {
		return "", errors.New("should not be called")
	}

	if _, _, err := exportTemplateRootfsReader(context.Background(), "tpl-1", "", "", 0); err == nil {
		t.Fatal("expected incomplete metadata error")
	}
}
