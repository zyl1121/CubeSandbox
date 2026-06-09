// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package templatecenter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
	cubeboxv1 "github.com/tencentcloud/CubeSandbox/CubeMaster/api/services/cubebox/v1"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/constants"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/db/models"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/base/log"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/cubelet"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/errorcode"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/localcache"
	sandboxtypes "github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
	"gorm.io/gorm"
)

// commitSandboxRPCTimeout caps how long runTemplateCommitJob waits on the
// cubelet CommitSandbox RPC. The detached background context has no deadline
// of its own, so a hung cubelet would otherwise leave the job stuck in
// SNAPSHOTTING forever. Typical successful snapshots complete in 5-15s; the
// generous 5 minute ceiling accommodates large memory footprints while still
// guaranteeing eventual failure resolution.
const commitSandboxRPCTimeout = 5 * time.Minute

// cleanupTemplateRPCTimeout bounds the best-effort cleanup RPC used when the
// commit job rolls back after a partial success.
const cleanupTemplateRPCTimeout = 1 * time.Minute

// JobPhaseSnapshotting / JobPhaseRegistering are declared with the rest of
// the JobPhase* set in template_image.go; they are referenced here without
// re-declaration to avoid duplicate constants.

func SubmitTemplateCommit(ctx context.Context, sandboxID, nodeID, nodeIP string, req *sandboxtypes.CreateCubeSandboxReq, downloadBaseURL string) (*sandboxtypes.TemplateImageJobInfo, error) {
	if !isReady() {
		return nil, ErrTemplateStoreNotInitialized
	}
	if req == nil || req.Request == nil || strings.TrimSpace(req.RequestID) == "" {
		return nil, errors.New("requestID is required for commit; retry should generate a new request id")
	}
	requestID := strings.TrimSpace(req.RequestID)
	createReq, templateID, err := NormalizeRequest(req)
	if err != nil {
		return nil, err
	}
	storedReq, err := normalizeStoredTemplateRequest(req)
	if err != nil {
		return nil, err
	}
	requestSnapshot, err := marshalTemplateCommitJobRequest(storedReq)
	if err != nil {
		return nil, err
	}
	jobID := uuid.New().String()
	attemptNo := int32(1)
	retryOfJobID := ""
	reusedExistingJob := false
	if err := withTemplateWriteLock(templateID, func() error {
		// Idempotency: the same (request_id, COMMIT) tuple uniquely identifies a
		// commit attempt. Reuse a prior job when payload matches; reject on drift.
		if job, err := getTemplateImageJobByRequestOperation(ctx, requestID, JobOperationCommit); err == nil {
			if job.RequestJSON == requestSnapshot {
				jobID = job.JobID
				reusedExistingJob = true
				return nil
			}
			return fmt.Errorf("%w: request_id=%s already used with a different commit payload (job_id=%s)", ErrTemplateAttemptInProgress, requestID, job.JobID)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		definitionFailed := false
		if def, err := GetDefinition(ctx, templateID); err == nil {
			if strings.EqualFold(def.Status, StatusFailed) {
				definitionFailed = true
			} else {
				return fmt.Errorf("template %s already exists; committed template specs are immutable", templateID)
			}
		} else if !errors.Is(err, ErrTemplateNotFound) {
			return err
		}

		if job, err := getActiveTemplateImageJobByTemplateID(ctx, templateID); err == nil {
			if job.RequestJSON == requestSnapshot {
				jobID = job.JobID
				reusedExistingJob = true
				return nil
			}
			return fmt.Errorf("%w: template %s is currently %s (job_id=%s)", ErrTemplateAttemptInProgress, templateID, strings.ToLower(job.Status), job.JobID)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var latestJob *models.TemplateImageJob
		if job, err := getLatestTemplateImageJobByTemplateID(ctx, templateID); err == nil {
			latestJob = job
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if definitionFailed {
			if err := cleanupTemplateReplicas(ctx, templateID); err != nil {
				return err
			}
			if err := cleanupTemplateMetadata(ctx, templateID); err != nil {
				return err
			}
		}

		if latestJob != nil {
			attemptNo = nextAttemptNoFromLatest(latestJob.AttemptNo)
			retryOfJobID = latestJob.JobID
		}
		record := newCommitTemplateImageJobRecord(jobID, requestID, templateID, nodeID, nodeIP, createReq, requestSnapshot, attemptNo, retryOfJobID)
		if createErr := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).Create(record).Error; createErr != nil {
			// Concurrent writer may have inserted the same (request_id, COMMIT)
			// tuple between our lookup and create. Fall back to idempotent reuse.
			if isDuplicateKeyError(createErr) {
				if job, lookupErr := getTemplateImageJobByRequestOperation(ctx, requestID, JobOperationCommit); lookupErr == nil {
					if job.RequestJSON == requestSnapshot {
						jobID = job.JobID
						reusedExistingJob = true
						return nil
					}
					return fmt.Errorf("%w: request_id=%s already used with a different commit payload (job_id=%s)", ErrTemplateAttemptInProgress, requestID, job.JobID)
				}
			}
			return createErr
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if reusedExistingJob {
		return GetTemplateImageJobInfo(ctx, jobID)
	}

	// runTemplateCommitJob historically ran synchronously on the HTTP handler
	// goroutine, which (a) blocked the caller for the full snapshot duration
	// (≈10–30s for a 2C2000M sandbox) defeating the asynchronous build_id +
	// poll contract introduced by PR #227, and (b) made any panic inside the
	// job invisible to the response. Match the sibling SubmitTemplateImage()
	// path: detach the context (so HTTP client disconnects do not cancel the
	// background work) and dispatch onto a fresh goroutine.
	go runTemplateCommitJob(detachTemplateImageJobContext(ctx, map[string]any{
		"job_id":          jobID,
		"template_id":     templateID,
		"attempt_no":      attemptNo,
		"retry_of_job_id": retryOfJobID,
		"sandbox_id":      sandboxID,
		"node_id":         nodeID,
		"node_ip":         nodeIP,
	}), jobID, sandboxID, nodeID, nodeIP, createReq, storedReq, downloadBaseURL)

	return GetTemplateImageJobInfo(ctx, jobID)
}

func runTemplateCommitJob(ctx context.Context, jobID, sandboxID, nodeID, nodeIP string, createReq, storedReq *sandboxtypes.CreateCubeSandboxReq, downloadBaseURL string) {
	templateID := createReq.Annotations[constants.CubeAnnotationAppSnapshotTemplateID]
	logger := log.G(ctx).WithFields(map[string]any{
		"job_id":      jobID,
		"template_id": templateID,
		"sandbox_id":  sandboxID,
		"node_id":     nodeID,
		"node_ip":     nodeIP,
	})
	requestSnapshot, requestSnapshotErr := marshalTemplateCommitJobRequest(storedReq)
	if requestSnapshotErr != nil {
		logger.Errorf("marshal template commit request snapshot fail: %v", requestSnapshotErr)
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseSnapshotting,
			"progress":      100,
			"error_message": requestSnapshotErr.Error(),
		})
		return
	}
	finishWithTemplateInfo := func(info *TemplateInfo, artifact *models.RootfsArtifact, expected, ready, failed int32) {
		if info == nil {
			_ = updateTemplateImageJob(ctx, jobID, map[string]any{
				"status":        JobStatusFailed,
				"phase":         JobPhaseRegistering,
				"progress":      100,
				"error_message": "template info is nil",
			})
			return
		}
		resultPayload, _ := json.Marshal(info)
		jobStatus := JobStatusReady
		jobPhase := JobPhaseReady
		if info.Status == StatusFailed {
			jobStatus = JobStatusFailed
			jobPhase = JobPhaseRegistering
		}
		values := map[string]any{
			"status":              jobStatus,
			"phase":               jobPhase,
			"progress":            100,
			"expected_node_count": expected,
			"ready_node_count":    ready,
			"failed_node_count":   failed,
			"template_status":     info.Status,
			"result_json":         string(resultPayload),
			"error_message":       info.LastError,
		}
		if artifact != nil {
			values["artifact_id"] = artifact.ArtifactID
			values["artifact_status"] = artifact.Status
			values["template_spec_fingerprint"] = artifact.TemplateSpecFingerprint
		}
		_ = updateTemplateImageJob(ctx, jobID, values)
	}
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			logger.Errorf("template commit job panic: %v\n%s", r, stack)
			_ = updateTemplateImageJob(ctx, jobID, map[string]any{
				"status":        JobStatusFailed,
				"phase":         JobPhaseSnapshotting,
				"progress":      100,
				"error_message": fmt.Sprintf("template commit job panic: %v", r),
			})
		}
	}()
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"status":   JobStatusRunning,
		"phase":    JobPhaseSnapshotting,
		"progress": 10,
	})

	commitCtx, commitCancel := context.WithTimeout(ctx, commitSandboxRPCTimeout)
	commitRsp, err := cubelet.CommitSandbox(commitCtx, cubelet.GetCubeletAddr(nodeIP), &cubeboxv1.CommitSandboxRequest{
		RequestID:   uuid.NewString(),
		SandboxID:   sandboxID,
		TemplateID:  templateID,
		SnapshotDir: createReq.SnapshotDir,
	})
	commitCancel()
	if err != nil {
		errMsg := fmt.Sprintf("cubelet CommitSandbox transport error: %v", err)
		logger.Errorf("%s", errMsg)
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseSnapshotting,
			"progress":      100,
			"error_message": errMsg,
		})
		return
	}
	// Cubelet returns errorcode.ErrorCode_Success (=200) on success, not 0.
	// All other call sites in CubeMaster compare against int(ErrorCode_Success);
	// this site is the only one that historically wrote `!= 0`, which silently
	// flipped every successful commit into a FAILED job with empty error_message.
	if ret := commitRsp.GetRet(); ret == nil || int(ret.GetRetCode()) != int(errorcode.ErrorCode_Success) {
		errMsg := buildCommitFailureMessage(commitRsp)
		logger.Errorf("cubelet CommitSandbox returned non-success: %s snapshot_path=%q", errMsg, commitRsp.GetSnapshotPath())
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":        JobStatusFailed,
			"phase":         JobPhaseSnapshotting,
			"progress":      100,
			"error_message": errMsg,
		})
		return
	}

	snapshotPath := commitRsp.GetSnapshotPath()
	_ = updateTemplateImageJob(ctx, jobID, map[string]any{
		"phase":         JobPhaseRegistering,
		"progress":      70,
		"node_id":       nodeID,
		"node_ip":       nodeIP,
		"snapshot_path": snapshotPath,
	})

	definitionCreated := false
	cleanupOnFailure := func(cause error) {
		if cause == nil {
			cause = errors.New("template commit job failed: unknown cause")
		}
		// v4+: master no longer passes physical SnapshotPath/Objects to
		// cubelet on cleanup. Cubelet resolves them from its local snapshot
		// catalog written during CommitSandbox (with deterministic fallback).
		// We always attempt cleanup (even before cubelet reports a snapshot
		// path) because partial-failure rollback may need to remove a half-
		// written entry. The bounded RPC timeout protects the goroutine from
		// a hung cubelet.
		cleanupCtx, cleanupCancel := context.WithTimeout(ctx, cleanupTemplateRPCTimeout)
		_, cleanupErr := cubelet.CleanupTemplate(cleanupCtx, cubelet.GetCubeletAddr(nodeIP), &cubeboxv1.CleanupTemplateRequest{
			RequestID:  uuid.NewString(),
			TemplateID: templateID,
		})
		cleanupCancel()
		if cleanupErr != nil {
			cause = errors.Join(cause, cleanupErr)
		}
		if definitionCreated {
			if cleanupErr := cleanupTemplateMetadata(ctx, templateID); cleanupErr != nil {
				cause = errors.Join(cause, cleanupErr)
			}
		}
		logger.Errorf("template commit job rolling back to FAILED: %v", cause)
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseRegistering,
			"progress":        100,
			"template_status": StatusFailed,
			"error_message":   cause.Error(),
		})
		invalidateTemplateCaches(templateID)
	}

	if err := createDefinition(ctx, templateID, storedReq, createReq.InstanceType, constants.GetAppSnapshotVersion(createReq.Annotations)); err != nil {
		cleanupOnFailure(err)
		return
	}
	definitionCreated = true
	if cacheErr := setTemplateRequestCache(templateID, storedReq); cacheErr != nil {
		logger.Warnf("set template request cache fail:%v", cacheErr)
	}

	replica := ReplicaStatus{
		NodeID:       nodeID,
		NodeIP:       nodeIP,
		InstanceType: createReq.InstanceType,
		Spec:         calculateRequestSpec(createReq),
		Status:       ReplicaStatusReady,
	}
	if err := UpsertReplica(ctx, templateID, createReq.InstanceType, replica); err != nil {
		cleanupOnFailure(err)
		return
	}
	setTemplateLocalityCache(templateID, []ReplicaStatus{replica})
	localcache.RegisterTemplateReplica(templateID, nodeID, 1)

	remoteTargets, err := resolveCommitRemoteTargets(createReq.InstanceType, createReq.DistributionScope, nodeID, nodeIP)
	if err != nil {
		if updateErr := UpdateDefinitionStatus(ctx, templateID, StatusPartiallyReady, err.Error()); updateErr != nil {
			logger.Errorf("update template status after remote target resolution failure fail: %v", updateErr)
			_ = updateTemplateImageJob(ctx, jobID, map[string]any{
				"status":          JobStatusFailed,
				"phase":           JobPhaseRegistering,
				"progress":        100,
				"template_status": StatusPartiallyReady,
				"error_message":   errors.Join(err, updateErr).Error(),
			})
			return
		}
		info, infoErr := GetTemplateInfo(ctx, templateID)
		if infoErr != nil {
			_ = updateTemplateImageJob(ctx, jobID, map[string]any{
				"status":          JobStatusFailed,
				"phase":           JobPhaseRegistering,
				"progress":        100,
				"template_status": StatusPartiallyReady,
				"error_message":   errors.Join(err, infoErr).Error(),
			})
			return
		}
		finishWithTemplateInfo(info, nil, 1, 1, 0)
		logger.Warnf("template commit finished with origin-only readiness: %v", err)
		return
	}

	expectedNodes := int32(1 + len(remoteTargets))
	if len(remoteTargets) == 0 {
		if err := UpdateDefinitionStatus(ctx, templateID, StatusReady, ""); err != nil {
			_ = updateTemplateImageJob(ctx, jobID, map[string]any{
				"status":          JobStatusFailed,
				"phase":           JobPhaseRegistering,
				"progress":        100,
				"template_status": StatusReady,
				"error_message":   err.Error(),
			})
			return
		}
		info, infoErr := GetTemplateInfo(ctx, templateID)
		if infoErr != nil {
			_ = updateTemplateImageJob(ctx, jobID, map[string]any{
				"status":          JobStatusFailed,
				"phase":           JobPhaseRegistering,
				"progress":        100,
				"template_status": StatusReady,
				"error_message":   infoErr.Error(),
			})
			return
		}
		finishWithTemplateInfo(info, nil, expectedNodes, 1, 0)
		logger.Infof("template commit job finished successfully on origin only")
		return
	}

	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"phase":               JobPhaseDistributing,
		"progress":            78,
		"expected_node_count": expectedNodes,
		"ready_node_count":    1,
		"failed_node_count":   0,
	}); err != nil {
		logger.Errorf("update commit distribution start fail: %v", err)
	}
	artifact, generatedReq, err := pullAndRegisterCommittedRootfsArtifact(ctx, nodeIP, commitRsp, templateID, storedReq, requestSnapshot, downloadBaseURL)
	if err != nil {
		statusErr := UpdateDefinitionStatus(ctx, templateID, StatusPartiallyReady, err.Error())
		persistErr := recordCommitDistributionFailure(ctx, templateID, createReq, remoteTargets, "", jobID, err)
		refreshErr := refreshTemplateReplicaSummary(ctx, templateID)
		info, infoErr := GetTemplateInfo(ctx, templateID)
		if infoErr != nil {
			err = errors.Join(err, statusErr, persistErr, refreshErr, infoErr)
			_ = updateTemplateImageJob(ctx, jobID, map[string]any{
				"status":          JobStatusFailed,
				"phase":           JobPhaseDistributing,
				"progress":        100,
				"template_status": StatusPartiallyReady,
				"artifact_status": ArtifactStatusFailed,
				"error_message":   err.Error(),
			})
			return
		}
		readyCount, failedCount := countTemplateReplicaStates(info.Replicas)
		if extraErr := errors.Join(err, statusErr, persistErr, refreshErr); extraErr != nil {
			info.LastError = extraErr.Error()
		}
		finishWithTemplateInfo(info, artifact, expectedNodes, readyCount, failedCount)
		logger.Warnf("template commit artifact registration failed; keeping origin replica: %v", err)
		return
	}
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"artifact_id":               artifact.ArtifactID,
		"artifact_status":           artifact.Status,
		"template_spec_fingerprint": artifact.TemplateSpecFingerprint,
		"phase":                     JobPhaseDistributing,
		"progress":                  84,
	}); err != nil {
		logger.Errorf("update commit artifact metadata fail: %v", err)
	}
	distReq := &sandboxtypes.CreateTemplateFromImageReq{
		InstanceType:      createReq.InstanceType,
		DistributionScope: createReq.DistributionScope,
		WritableLayerSize: artifact.WritableLayerSize,
	}
	readyTargets, _, readyRemote, failedRemote, distErr := distributeRootfsArtifact(ctx, distReq, generatedReq, artifact, templateID, jobID)
	if err := updateTemplateImageJob(ctx, jobID, map[string]any{
		"phase":               JobPhaseRegistering,
		"progress":            90,
		"expected_node_count": expectedNodes,
		"ready_node_count":    1 + readyRemote,
		"failed_node_count":   failedRemote,
		"artifact_status":     artifact.Status,
		"error_message":       errorString(distErr),
	}); err != nil {
		logger.Errorf("update commit distribution result fail: %v", err)
	}
	_, persistErr := createTemplateReplicasOnNodes(ctx, templateID, generatedReq, readyTargets, replicaRunOptions{
		ArtifactID: artifact.ArtifactID,
		JobID:      jobID,
	})
	if persistErr != nil {
		logger.Errorf("create remote template replicas fail: %v", persistErr)
	}
	if err := refreshTemplateReplicaSummary(ctx, templateID); err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseRegistering,
			"progress":        100,
			"template_status": StatusPartiallyReady,
			"artifact_id":     artifact.ArtifactID,
			"artifact_status": artifact.Status,
			"error_message":   errors.Join(distErr, persistErr, err).Error(),
		})
		return
	}
	info, err := GetTemplateInfo(ctx, templateID)
	if err != nil {
		_ = updateTemplateImageJob(ctx, jobID, map[string]any{
			"status":          JobStatusFailed,
			"phase":           JobPhaseRegistering,
			"progress":        100,
			"template_status": StatusPartiallyReady,
			"artifact_id":     artifact.ArtifactID,
			"artifact_status": artifact.Status,
			"error_message":   errors.Join(distErr, persistErr, err).Error(),
		})
		return
	}
	readyCount, failedCount := countTemplateReplicaStates(info.Replicas)
	finishWithTemplateInfo(info, artifact, expectedNodes, readyCount, failedCount)
	if distErr != nil || persistErr != nil {
		logger.Warnf("template commit finished partially ready: distributionErr=%v persistErr=%v", distErr, persistErr)
		return
	}
	logger.Infof("template commit job finished successfully")
}

// buildCommitFailureMessage produces a never-empty error message for the failure
// branch of runTemplateCommitJob. Cubelet's CommitSandbox sometimes returns a
// non-success Ret with empty RetMsg; the previous implementation silently
// overwrote the fallback message with that empty string and stored an empty
// error_message in t_cube_template_image_job, making post-mortem impossible.
func buildCommitFailureMessage(commitRsp *cubeboxv1.CommitSandboxResponse) string {
	if commitRsp == nil {
		return "commit sandbox failed: cubelet returned nil response"
	}
	ret := commitRsp.GetRet()
	if ret == nil {
		return "commit sandbox failed: cubelet returned empty Ret"
	}
	retCode := int(ret.GetRetCode())
	retMsg := strings.TrimSpace(ret.GetRetMsg())
	if retMsg == "" {
		return fmt.Sprintf("commit sandbox failed: retCode=%d (empty retMsg)", retCode)
	}
	return fmt.Sprintf("commit sandbox failed: retCode=%d retMsg=%s", retCode, retMsg)
}

func newCommitTemplateImageJobRecord(
	jobID, requestID, templateID, nodeID, nodeIP string,
	createReq *sandboxtypes.CreateCubeSandboxReq,
	requestSnapshot string,
	attemptNo int32,
	retryOfJobID string,
) *models.TemplateImageJob {
	return &models.TemplateImageJob{
		JobID:                   jobID,
		TemplateID:              templateID,
		RequestID:               requestID,
		AttemptNo:               attemptNo,
		RetryOfJobID:            retryOfJobID,
		Operation:               JobOperationCommit,
		NodeID:                  nodeID,
		NodeIP:                  nodeIP,
		TemplateSpecFingerprint: buildCommitTemplateSpecFingerprintFromSnapshot(requestSnapshot),
		InstanceType:            createReq.InstanceType,
		NetworkType:             createReq.NetworkType,
		Status:                  JobStatusPending,
		Phase:                   JobPhaseSnapshotting,
		Progress:                0,
		RequestJSON:             requestSnapshot,
	}
}

func getTemplateImageJobByRequestOperation(ctx context.Context, requestID, operation string) (*models.TemplateImageJob, error) {
	record := &models.TemplateImageJob{}
	err := store.db.WithContext(ctx).Table(constants.TemplateImageJobTableName).
		Where("request_id = ? AND operation = ?", requestID, operation).
		Order("id desc").First(record).Error
	if err != nil {
		return nil, err
	}
	return record, nil
}

func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "Duplicate entry") || strings.Contains(msg, "1062")
}
