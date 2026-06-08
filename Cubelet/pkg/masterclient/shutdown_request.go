package masterclient

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ShutdownStatusReason  = "CubeletStopping"
	ShutdownStatusMessage = "Cubelet is shutting down"
)

func NewShutdownStatusRequest(now time.Time) *UpdateNodeStatusRequest {
	currentTime := metav1.NewTime(now)
	return &UpdateNodeStatusRequest{
		Conditions: []corev1.NodeCondition{
			{
				Type:               corev1.NodeReady,
				Status:             corev1.ConditionFalse,
				Reason:             ShutdownStatusReason,
				Message:            ShutdownStatusMessage,
				LastHeartbeatTime:  currentTime,
				LastTransitionTime: currentTime,
			},
		},
		HeartbeatTime: now,
	}
}
