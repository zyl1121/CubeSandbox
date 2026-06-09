// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cubebox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	cubebox "github.com/tencentcloud/CubeSandbox/Cubelet/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/api/services/errorcode/v1"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/log"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/pathutil"
	"github.com/tencentcloud/CubeSandbox/Cubelet/storage"
)

const exportTemplateRootfsChunkSize = 1 << 20

var (
	getLocalSnapshotForExport  = storage.GetLocalSnapshot
	resolveCowDevPathForExport = storage.ResolveCowDevPath
)

func (s *service) ExportTemplateRootfsArtifact(req *cubebox.ExportTemplateRootfsArtifactRequest, stream cubebox.CubeboxMgr_ExportTemplateRootfsArtifactServer) error {
	rsp := &cubebox.ExportTemplateRootfsArtifactChunk{
		RequestID:  req.GetRequestID(),
		TemplateID: strings.TrimSpace(req.GetTemplateID()),
		Ret:        &errorcode.Ret{RetCode: errorcode.ErrorCode_Success},
	}
	if rsp.TemplateID == "" {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = "templateID is required"
		return stream.Send(rsp)
	}
	if err := pathutil.ValidateSafeID(rsp.TemplateID); err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_InvalidParamFormat
		rsp.Ret.RetMsg = fmt.Sprintf("invalid templateID: %v", err)
		return stream.Send(rsp)
	}

	reader, sizeBytes, err := exportTemplateRootfsReader(stream.Context(), rsp.TemplateID, req.GetRootfsVol(), req.GetRootfsKind(), req.GetRootfsSizeBytes())
	if err != nil {
		rsp.Ret.RetCode = errorcode.ErrorCode_PreConditionFailed
		rsp.Ret.RetMsg = err.Error()
		return stream.Send(rsp)
	}
	defer reader.Close()
	rsp.RootfsSizeBytes = sizeBytes
	if err := stream.Send(rsp); err != nil {
		return err
	}

	limited := io.LimitReader(reader, int64(sizeBytes))
	buf := make([]byte, exportTemplateRootfsChunkSize)
	for {
		n, readErr := limited.Read(buf)
		if n > 0 {
			if err := stream.Send(&cubebox.ExportTemplateRootfsArtifactChunk{
				RequestID:       rsp.RequestID,
				TemplateID:      rsp.TemplateID,
				RootfsSizeBytes: sizeBytes,
				Content:         append([]byte(nil), buf[:n]...),
			}); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func exportTemplateRootfsReader(ctx context.Context, templateID, fallbackRootfsVol, fallbackRootfsKind string, fallbackRootfsSizeBytes uint64) (io.ReadCloser, uint64, error) {
	rootfsVol := strings.TrimSpace(fallbackRootfsVol)
	rootfsKind := strings.TrimSpace(fallbackRootfsKind)
	rootfsSizeBytes := fallbackRootfsSizeBytes

	entry, err := getLocalSnapshotForExport(ctx, templateID)
	switch {
	case err == nil && entry != nil:
		if v := strings.TrimSpace(entry.RootfsVol); v != "" {
			rootfsVol = v
		}
		if v := strings.TrimSpace(entry.RootfsKind); v != "" {
			rootfsKind = v
		}
		if entry.RootfsSizeBytes > 0 {
			rootfsSizeBytes = entry.RootfsSizeBytes
		}
	case errors.Is(err, storage.ErrSnapshotCatalogNotFound):
		log.G(ctx).Warnf("ExportTemplateRootfsArtifact %s: catalog miss; falling back to request metadata", templateID)
	case err != nil:
		return nil, 0, fmt.Errorf("lookup local snapshot %s: %w", templateID, err)
	}

	if rootfsVol == "" || rootfsKind == "" {
		return nil, 0, fmt.Errorf("template %s rootfs metadata is incomplete", templateID)
	}
	if rootfsSizeBytes == 0 {
		return nil, 0, fmt.Errorf("template %s rootfs size is missing", templateID)
	}
	if rootfsSizeBytes > math.MaxInt64 {
		return nil, 0, fmt.Errorf("template %s rootfs size %d exceeds supported stream limit", templateID, rootfsSizeBytes)
	}

	devPath, err := resolveCowDevPathForExport(ctx, rootfsVol, rootfsKind)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve rootfs device for %s: %w", templateID, err)
	}
	reader, err := os.Open(devPath) // NOCC:Path Traversal()
	if err != nil {
		return nil, 0, fmt.Errorf("open rootfs device %s: %w", devPath, err)
	}
	return reader, rootfsSizeBytes, nil
}
