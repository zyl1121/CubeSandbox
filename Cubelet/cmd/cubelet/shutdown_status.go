package main

import (
	"context"
	"fmt"
	"time"

	cubeletconfig "github.com/tencentcloud/CubeSandbox/Cubelet/pkg/config"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/masterclient"
	"github.com/tencentcloud/CubeSandbox/Cubelet/pkg/utils"
)

const shutdownStatusReportTimeout = 3 * time.Second

type shutdownStatusClient interface {
	UpdateNodeStatus(ctx context.Context, nodeID string, req *masterclient.UpdateNodeStatusRequest) error
}

var newShutdownStatusClient = func(endpoint string, timeout time.Duration) shutdownStatusClient {
	return masterclient.New(endpoint, timeout)
}

func ReportShutdownStatus(ctx context.Context) error {
	cfg := cubeletconfig.GetConfig()
	if cfg == nil || cfg.MetaServerConfig == nil || cfg.MetaServerConfig.MetaServerEndpoint == "" {
		return nil
	}

	identity, err := utils.GetHostIdentity()
	if err != nil {
		return fmt.Errorf("get host identity: %w", err)
	}
	if identity.InstanceID == "" {
		return fmt.Errorf("host identity instance id is empty")
	}

	return reportShutdownStatus(
		ctx,
		"http://"+cfg.MetaServerConfig.MetaServerEndpoint,
		identity.InstanceID,
		shutdownStatusReportTimeout,
		time.Now,
	)
}

func reportShutdownStatus(ctx context.Context, endpoint, nodeID string, timeout time.Duration, now func() time.Time) error {
	if endpoint == "" {
		return nil
	}
	if nodeID == "" {
		return fmt.Errorf("node id is empty")
	}
	if timeout <= 0 {
		timeout = shutdownStatusReportTimeout
	}
	if now == nil {
		now = time.Now
	}

	reportCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return newShutdownStatusClient(endpoint, timeout).UpdateNodeStatus(reportCtx, nodeID, masterclient.NewShutdownStatusRequest(now()))
}
