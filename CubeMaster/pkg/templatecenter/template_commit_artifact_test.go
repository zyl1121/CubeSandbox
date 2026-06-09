// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"testing"

	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	imagev1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/images/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestBuildCommitReplicaCreateRequestRewritesRootfsImage(t *testing.T) {
	sourceReq := &types.CreateCubeSandboxReq{
		Volumes: []*types.Volume{
			{
				Name: "rootfs",
				VolumeSource: &types.VolumeSource{
					EmptyDir: &types.EmptyDirVolumeSource{SizeLimit: "20Gi"},
				},
			},
			{
				Name: "data",
				VolumeSource: &types.VolumeSource{
					EmptyDir: &types.EmptyDirVolumeSource{SizeLimit: "2Gi"},
				},
			},
		},
		Containers: []*types.Container{
			{
				Name: "main",
				Image: &types.ImageSpec{
					Image:             "old-image",
					WritableLayerSize: "1Gi",
					Annotations: map[string]string{
						"keep": "me",
					},
				},
				VolumeMounts: []*cubeboxv1.VolumeMounts{
					{Name: "rootfs", ContainerPath: "/"},
				},
			},
			{
				Name: "sidecar",
				Image: &types.ImageSpec{
					Image: "sidecar-image",
				},
				VolumeMounts: []*cubeboxv1.VolumeMounts{
					{Name: "data", ContainerPath: "/data"},
				},
			},
		},
		Annotations: map[string]string{
			constants.CubeAnnotationAppSnapshotTemplateVersion: DefaultTemplateVersion,
			constants.CubeAnnotationsSystemDiskSize:            "20",
		},
		InstanceType: "cubebox",
	}
	artifact := &models.RootfsArtifact{
		ArtifactID:              "rfs-tpl-123",
		TemplateSpecFingerprint: "fp-123",
		Ext4SHA256:              "sha256-123",
		Ext4SizeBytes:           4096,
		DownloadToken:           "token-123",
		WritableLayerSize:       "20Gi",
	}

	got, err := buildCommitReplicaCreateRequest(sourceReq, "tpl-commit-123", artifact, "http://127.0.0.1:3000")
	if err != nil {
		t.Fatalf("buildCommitReplicaCreateRequest failed: %v", err)
	}
	if got.Request == nil || got.RequestID == "" {
		t.Fatal("expected generated request to carry a fresh request id")
	}
	if got.Annotations[constants.CubeAnnotationAppSnapshotTemplateID] != "tpl-commit-123" {
		t.Fatalf("template annotation mismatch: %q", got.Annotations[constants.CubeAnnotationAppSnapshotTemplateID])
	}
	if got.Annotations[constants.CubeAnnotationsAppSnapshotCreate] != "true" {
		t.Fatalf("expected %s=true", constants.CubeAnnotationsAppSnapshotCreate)
	}
	if got.Annotations[constants.CubeAnnotationRootfsArtifactID] != artifact.ArtifactID {
		t.Fatalf("artifact annotation mismatch: %q", got.Annotations[constants.CubeAnnotationRootfsArtifactID])
	}
	rootImage := got.Containers[0].Image
	if rootImage.Image != artifact.ArtifactID {
		t.Fatalf("root container image=%q, want artifact id %q", rootImage.Image, artifact.ArtifactID)
	}
	if rootImage.StorageMedia != imagev1.ImageStorageMediaType_ext4.String() {
		t.Fatalf("root image storage media=%q", rootImage.StorageMedia)
	}
	if rootImage.WritableLayerSize != "20Gi" {
		t.Fatalf("root writable layer size=%q", rootImage.WritableLayerSize)
	}
	if rootImage.Annotations[constants.CubeAnnotationRootfsArtifactURL] != "http://127.0.0.1:3000/cube/template/artifact/download?artifact_id=rfs-tpl-123&token=token-123" {
		t.Fatalf("root artifact url mismatch: %q", rootImage.Annotations[constants.CubeAnnotationRootfsArtifactURL])
	}
	if rootImage.Annotations["keep"] != "me" {
		t.Fatalf("expected existing image annotations to be preserved, got %#v", rootImage.Annotations)
	}
	if got.Containers[1].Image.Image != "sidecar-image" {
		t.Fatalf("non-rootfs container image should remain unchanged, got %q", got.Containers[1].Image.Image)
	}
}

func TestCommitWritableLayerSizePrefersRootVolumeLimit(t *testing.T) {
	req := &types.CreateCubeSandboxReq{
		Volumes: []*types.Volume{
			{
				Name: "rootfs",
				VolumeSource: &types.VolumeSource{
					EmptyDir: &types.EmptyDirVolumeSource{SizeLimit: "12Gi"},
				},
			},
		},
		Containers: []*types.Container{
			{
				Image: &types.ImageSpec{WritableLayerSize: "4Gi"},
				VolumeMounts: []*cubeboxv1.VolumeMounts{
					{Name: "rootfs", ContainerPath: "/"},
				},
			},
		},
	}
	if got := commitWritableLayerSize(req); got != "12Gi" {
		t.Fatalf("commitWritableLayerSize=%q, want %q", got, "12Gi")
	}
}
