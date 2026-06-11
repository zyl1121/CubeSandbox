// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package cube

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tencentcloud/CubeSandbox/CubeMaster/pkg/service/sandbox/types"
)

func TestApplyRequestEnvVarsToFirstContainerOverridesExistingEnvVars(t *testing.T) {
	req := &types.CreateCubeSandboxReq{
		EnvVars: map[string]string{
			"Z_KEY":    "last",
			"A_KEY":    "first",
			"EXISTING": "override",
			"  ":       "ignored",
		},
		Containers: []*types.Container{
			{
				Envs: []*types.KeyValue{
					{Key: "EXISTING", Value: "base"},
					{Key: "TEMPLATE_ONLY", Value: "keep"},
				},
			},
		},
	}

	applyRequestEnvVarsToFirstContainer(req)

	if assert.Len(t, req.Containers, 1) {
		envs := req.Containers[0].Envs
		if assert.Len(t, envs, 4) {
			assert.Equal(t, "EXISTING", envs[0].Key)
			assert.Equal(t, "override", envs[0].Value)
			assert.Equal(t, "TEMPLATE_ONLY", envs[1].Key)
			assert.Equal(t, "keep", envs[1].Value)
			assert.Equal(t, "A_KEY", envs[2].Key)
			assert.Equal(t, "first", envs[2].Value)
			assert.Equal(t, "Z_KEY", envs[3].Key)
			assert.Equal(t, "last", envs[3].Value)
		}
	}
	assert.Nil(t, req.EnvVars)
}
