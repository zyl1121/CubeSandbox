// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	imagev1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/node"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

var exportTemplateRootfsArtifactOnCubelet = cubelet.ExportTemplateRootfsArtifact

func pullAndRegisterCommittedRootfsArtifact(
	ctx context.Context,
	nodeIP string,
	commitRsp *cubeboxv1.CommitSandboxResponse,
	templateID string,
	sourceReq *sandboxtypes.CreateCubeSandboxReq,
	requestSnapshot string,
	downloadBaseURL string,
) (*models.RootfsArtifact, *sandboxtypes.CreateCubeSandboxReq, error) {
	artifactID := buildCommitArtifactID(templateID)
	record := &models.RootfsArtifact{
		ArtifactID:              artifactID,
		TemplateSpecFingerprint: buildCommitTemplateSpecFingerprintFromSnapshot(requestSnapshot),
		MasterNodeIP:            normalizeBaseURL(downloadBaseURL),
		WritableLayerSize:       commitWritableLayerSize(sourceReq),
		Status:                  ArtifactStatusBuilding,
		GCDeadline:              time.Now().Add(defaultTemplateArtifactTTL).Unix(),
	}
	if err := store.db.WithContext(ctx).Table(constants.RootfsArtifactTableName).Create(record).Error; err != nil {
		return nil, nil, err
	}

	storeDir, err := resolveArtifactStoreDir(ctx, record.ArtifactID)
	if err != nil {
		record.Status = ArtifactStatusFailed
		record.LastError = err.Error()
		_ = updateRootfsArtifact(ctx, record.ArtifactID, map[string]any{
			"status":     ArtifactStatusFailed,
			"last_error": err.Error(),
		})
		return record, nil, err
	}
	ext4Path := filepath.Join(storeDir, record.ArtifactID+".ext4")
	shaValue, sizeBytes, err := pullCommittedRootfsArtifact(ctx, nodeIP, commitRsp, templateID, ext4Path)
	if err != nil {
		record.Ext4Path = ext4Path
		record.Status = ArtifactStatusFailed
		record.LastError = err.Error()
		_ = updateRootfsArtifact(ctx, record.ArtifactID, map[string]any{
			"ext4_path":  ext4Path,
			"status":     ArtifactStatusFailed,
			"last_error": err.Error(),
		})
		return record, nil, err
	}
	record.MasterNodeIP = normalizeBaseURL(downloadBaseURL)
	record.Ext4Path = ext4Path
	record.Ext4SHA256 = shaValue
	record.Ext4SizeBytes = sizeBytes
	record.DownloadToken = uuid.NewString()

	generatedReq, err := buildCommitReplicaCreateRequest(sourceReq, templateID, record, downloadBaseURL)
	if err != nil {
		record.Status = ArtifactStatusFailed
		record.LastError = err.Error()
		_ = updateRootfsArtifact(ctx, record.ArtifactID, map[string]any{
			"ext4_path":       ext4Path,
			"ext4_sha256":     shaValue,
			"ext4_size_bytes": sizeBytes,
			"status":          ArtifactStatusFailed,
			"last_error":      err.Error(),
		})
		return record, nil, err
	}
	reqPayload, err := json.Marshal(generatedReq)
	if err != nil {
		record.Status = ArtifactStatusFailed
		record.LastError = err.Error()
		_ = updateRootfsArtifact(ctx, record.ArtifactID, map[string]any{
			"ext4_path":       ext4Path,
			"ext4_sha256":     shaValue,
			"ext4_size_bytes": sizeBytes,
			"status":          ArtifactStatusFailed,
			"last_error":      err.Error(),
		})
		return record, nil, err
	}
	record.Status = ArtifactStatusReady
	record.LastError = ""
	if err := updateRootfsArtifact(ctx, record.ArtifactID, map[string]any{
		"master_node_ip":         record.MasterNodeIP,
		"ext4_path":              ext4Path,
		"ext4_sha256":            shaValue,
		"ext4_size_bytes":        sizeBytes,
		"generated_request_json": string(reqPayload),
		"download_token":         record.DownloadToken,
		"status":                 ArtifactStatusReady,
		"last_error":             "",
		"gc_deadline":            record.GCDeadline,
	}); err != nil {
		return record, nil, err
	}
	latest, err := getRootfsArtifactByID(ctx, record.ArtifactID)
	if err != nil {
		return record, nil, err
	}
	return latest, generatedReq, nil
}

func pullCommittedRootfsArtifact(
	ctx context.Context,
	nodeIP string,
	commitRsp *cubeboxv1.CommitSandboxResponse,
	templateID, ext4Path string,
) (string, int64, error) {
	if commitRsp == nil {
		return "", 0, fmt.Errorf("commit response is nil")
	}
	storeDir := filepath.Dir(ext4Path)
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return "", 0, err
	}
	tmpPath := ext4Path + ".download"
	if err := os.RemoveAll(tmpPath); err != nil { // NOCC:Path Traversal()
		return "", 0, err
	}
	stream, closeFn, err := exportTemplateRootfsArtifactOnCubelet(ctx, cubelet.GetCubeletAddr(nodeIP), &cubeboxv1.ExportTemplateRootfsArtifactRequest{
		RequestID:       uuid.NewString(),
		TemplateID:      templateID,
		RootfsVol:       commitRsp.GetRootfsVol(),
		RootfsKind:      commitRsp.GetRootfsKind(),
		RootfsSizeBytes: commitRsp.GetRootfsSizeBytes(),
	})
	if err != nil {
		return "", 0, err
	}
	defer closeFn()

	first, err := stream.Recv()
	if err != nil {
		return "", 0, err
	}
	if first.GetRet() == nil || int(first.GetRet().GetRetCode()) != int(errorcode.ErrorCode_Success) {
		if first.GetRet() != nil && strings.TrimSpace(first.GetRet().GetRetMsg()) != "" {
			return "", 0, fmt.Errorf("export template rootfs failed: %s", first.GetRet().GetRetMsg())
		}
		return "", 0, fmt.Errorf("export template rootfs failed: empty response")
	}
	if first.GetRootfsSizeBytes() == 0 {
		return "", 0, fmt.Errorf("export template rootfs failed: rootfs size is zero")
	}

	file, err := os.Create(tmpPath) // NOCC:Path Traversal()
	if err != nil {
		return "", 0, err
	}
	defer func() {
		_ = file.Close()
		if err != nil {
			_ = os.Remove(tmpPath) // NOCC:Path Traversal()
		}
	}()
	hasher := sha256.New()
	totalBytes := int64(0)
	writeChunk := func(chunk []byte) error {
		if len(chunk) == 0 {
			return nil
		}
		n, writeErr := io.MultiWriter(file, hasher).Write(chunk)
		totalBytes += int64(n)
		return writeErr
	}
	if err := writeChunk(first.GetContent()); err != nil {
		return "", 0, err
	}
	for {
		chunk, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			return "", 0, recvErr
		}
		if err := writeChunk(chunk.GetContent()); err != nil {
			return "", 0, err
		}
	}
	if totalBytes != int64(first.GetRootfsSizeBytes()) {
		return "", 0, fmt.Errorf("exported rootfs size mismatch: got %d want %d", totalBytes, first.GetRootfsSizeBytes())
	}
	if err := file.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmpPath, ext4Path); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), totalBytes, nil
}

func buildCommitReplicaCreateRequest(sourceReq *sandboxtypes.CreateCubeSandboxReq, templateID string, artifact *models.RootfsArtifact, downloadBaseURL string) (*sandboxtypes.CreateCubeSandboxReq, error) {
	if sourceReq == nil {
		return nil, fmt.Errorf("source request is nil")
	}
	if artifact == nil {
		return nil, fmt.Errorf("artifact is nil")
	}
	cloned, err := cloneCreateRequest(sourceReq)
	if err != nil {
		return nil, err
	}
	if cloned.Annotations == nil {
		cloned.Annotations = make(map[string]string)
	}
	constants.SetAppSnapshotVersion(cloned.Annotations, DefaultTemplateVersion)
	cloned.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] = templateID
	cloned.Annotations[constants.CubeAnnotationsAppSnapshotCreate] = "true"
	cloned.Annotations[constants.CubeAnnotationRootfsArtifactID] = artifact.ArtifactID
	if artifact.TemplateSpecFingerprint != "" {
		cloned.Annotations[constants.CubeAnnotationTemplateSpecFingerprint] = artifact.TemplateSpecFingerprint
	}

	writableLayerSize := commitWritableLayerSize(cloned)
	if writableLayerSize == "" {
		return nil, fmt.Errorf("commit request is missing writable layer size")
	}
	cloned.Annotations[constants.CubeAnnotationWritableLayerSize] = writableLayerSize
	if sizeGi, convErr := quantityToGi(writableLayerSize); convErr == nil && sizeGi > 0 {
		cloned.Annotations[constants.CubeAnnotationsSystemDiskSize] = strconv.FormatInt(sizeGi, 10)
	}
	rootVolumeName, err := commitRootfsVolumeName(cloned)
	if err != nil {
		return nil, err
	}

	replaced := false
	for _, ctr := range cloned.Containers {
		if ctr == nil || ctr.Image == nil {
			continue
		}
		if !containerUsesRootfsVolume(ctr, rootVolumeName) {
			continue
		}
		imageAnnotations := cloneStringMap(ctr.Image.Annotations)
		imageAnnotations[constants.CubeAnnotationRootfsArtifactID] = artifact.ArtifactID
		imageAnnotations[constants.CubeAnnotationRootfsArtifactURL] = buildDownloadURL(downloadBaseURL, artifact.ArtifactID, artifact.DownloadToken)
		imageAnnotations[constants.CubeAnnotationRootfsArtifactToken] = artifact.DownloadToken
		imageAnnotations[constants.CubeAnnotationRootfsArtifactSHA256] = artifact.Ext4SHA256
		imageAnnotations[constants.CubeAnnotationRootfsArtifactSizeBytes] = strconv.FormatInt(artifact.Ext4SizeBytes, 10)
		imageAnnotations[constants.CubeAnnotationWritableLayerSize] = writableLayerSize
		if artifact.TemplateSpecFingerprint != "" {
			imageAnnotations[constants.CubeAnnotationTemplateSpecFingerprint] = artifact.TemplateSpecFingerprint
		}
		ctr.Image = &sandboxtypes.ImageSpec{
			Image:             artifact.ArtifactID,
			Annotations:       imageAnnotations,
			StorageMedia:      imagev1.ImageStorageMediaType_ext4.String(),
			WritableLayerSize: writableLayerSize,
		}
		replaced = true
	}
	if !replaced {
		return nil, fmt.Errorf("commit request has no rootfs-mounted container to replace")
	}
	cloned.Request = &sandboxtypes.Request{RequestID: uuid.NewString()}
	return cloned, nil
}

func recordCommitDistributionFailure(ctx context.Context, templateID string, req *sandboxtypes.CreateCubeSandboxReq, targets []*node.Node, artifactID, jobID string, cause error) error {
	if req == nil || len(targets) == 0 {
		return nil
	}
	message := "template distribution failed"
	if cause != nil {
		message = cause.Error()
	}
	var result error
	for _, target := range targets {
		if target == nil {
			continue
		}
		replica := buildReplicaForDistribution(target, req, artifactID, jobID)
		replica.Phase = ReplicaPhaseFailed
		replica.ErrorMessage = message
		replica.CleanupRequired = false
		if upsertErr := UpsertReplica(ctx, templateID, req.InstanceType, replica); upsertErr != nil {
			result = errors.Join(result, fmt.Errorf("upsert distribution failure replica for node %s: %w", target.ID(), upsertErr))
		}
	}
	return result
}

func buildCommitArtifactID(templateID string) string {
	return "rfs-" + strings.TrimSpace(templateID)
}

func commitWritableLayerSize(req *sandboxtypes.CreateCubeSandboxReq) string {
	rootVolumeName, err := commitRootfsVolumeName(req)
	if err == nil {
		for _, volume := range req.Volumes {
			if volume == nil || volume.Name != rootVolumeName || volume.VolumeSource == nil || volume.VolumeSource.EmptyDir == nil {
				continue
			}
			if value := strings.TrimSpace(volume.VolumeSource.EmptyDir.SizeLimit); value != "" {
				return value
			}
		}
	}
	for _, ctr := range req.Containers {
		if ctr == nil || ctr.Image == nil {
			continue
		}
		if value := strings.TrimSpace(ctr.Image.WritableLayerSize); value != "" {
			return value
		}
	}
	sizeGi := strings.TrimSpace(req.Annotations[constants.CubeAnnotationsSystemDiskSize])
	if sizeGi != "" {
		return sizeGi + "Gi"
	}
	return ""
}

func commitRootfsVolumeName(req *sandboxtypes.CreateCubeSandboxReq) (string, error) {
	if req == nil {
		return "", fmt.Errorf("request is nil")
	}
	rootVolumeName := ""
	for _, ctr := range req.Containers {
		if ctr == nil {
			continue
		}
		for _, mount := range ctr.VolumeMounts {
			if mount == nil || mount.ContainerPath != "/" {
				continue
			}
			if rootVolumeName != "" && rootVolumeName != mount.Name {
				return "", fmt.Errorf("multiple rootfs volume mounts found: %s and %s", rootVolumeName, mount.Name)
			}
			rootVolumeName = mount.Name
		}
	}
	if rootVolumeName == "" {
		return "", fmt.Errorf("commit request has no writable rootfs volume mount")
	}
	return rootVolumeName, nil
}

func containerUsesRootfsVolume(ctr *sandboxtypes.Container, rootVolumeName string) bool {
	if ctr == nil {
		return false
	}
	for _, mount := range ctr.VolumeMounts {
		if mount == nil || mount.ContainerPath != "/" {
			continue
		}
		if rootVolumeName == "" || mount.Name == rootVolumeName {
			return true
		}
	}
	return false
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func resolveCommitRemoteTargets(instanceType string, scope []string, originNodeID, originNodeIP string) ([]*node.Node, error) {
	targets, err := resolveTemplateNodes(instanceType, scope)
	if err != nil {
		return nil, err
	}
	out := make([]*node.Node, 0, len(targets))
	for _, target := range targets {
		if target == nil {
			continue
		}
		if (originNodeID != "" && target.ID() == originNodeID) || (originNodeIP != "" && target.HostIP() == originNodeIP) {
			continue
		}
		out = append(out, target)
	}
	return out, nil
}

func countTemplateReplicaStates(replicas []ReplicaStatus) (ready, failed int32) {
	for _, replica := range replicas {
		if replica.Status == ReplicaStatusReady {
			ready++
			continue
		}
		failed++
	}
	return ready, failed
}
