// Copyright (c) 2024 Tencent Inc.
// SPDX-License-Identifier: Apache-2.0
//

package metadata

import "github.com/urfave/cli/v2"

var Command = &cli.Command{
	Name:    "meta",
	Aliases: []string{"m"},
	Usage:   "inspect metadata; this command is read-only and must not be used to update metadata",
	Subcommands: cli.Commands{
		&ShowDbCommand,
		&ListDbCommand,
	},
}
