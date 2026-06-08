package masterclient

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestNewShutdownStatusRequestBuildsNotReadyCondition(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()

	req := NewShutdownStatusRequest(now)
	if req == nil {
		t.Fatal("NewShutdownStatusRequest returned nil")
	}
	if !req.HeartbeatTime.Equal(now) {
		t.Fatalf("HeartbeatTime=%v want %v", req.HeartbeatTime, now)
	}
	if len(req.Conditions) != 1 {
		t.Fatalf("conditions len=%d want 1", len(req.Conditions))
	}

	cond := req.Conditions[0]
	if cond.Type != corev1.NodeReady {
		t.Fatalf("condition type=%s want %s", cond.Type, corev1.NodeReady)
	}
	if cond.Status != corev1.ConditionFalse {
		t.Fatalf("condition status=%s want %s", cond.Status, corev1.ConditionFalse)
	}
	if cond.Reason != ShutdownStatusReason {
		t.Fatalf("condition reason=%s want %s", cond.Reason, ShutdownStatusReason)
	}
	if cond.Message != ShutdownStatusMessage {
		t.Fatalf("condition message=%q want %q", cond.Message, ShutdownStatusMessage)
	}
	if cond.LastHeartbeatTime.Time.IsZero() || cond.LastTransitionTime.Time.IsZero() {
		t.Fatalf("condition timestamps should be populated: %+v", cond)
	}
}
