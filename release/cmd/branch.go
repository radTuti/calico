// Copyright (c) 2024-2026 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"

	cli "github.com/urfave/cli/v3"

	"github.com/projectcalico/calico/release/internal/defaults"
	"github.com/projectcalico/calico/release/pkg/manager/calico"
)

// The branch command suite is used to manage branches.
func branchCommand(cfg *Config) *cli.Command {
	return &cli.Command{
		Name:     "branch",
		Aliases:  []string{"br"},
		Usage:    "Manage branches.",
		Commands: branchSubCommands(cfg),
	}
}

var branchSubCommands = func(cfg *Config) []*cli.Command {
	return []*cli.Command{
		branchCutCommand(cfg),
	}
}

var (
	branchCutFlags = []cli.Flag{
		orgFlag,
		repoFlag,
		repoRemoteFlag,
		mainBranchFlag,
		releaseBranchPrefixFlag,
		devTagSuffixFlag,
		localFlag,
		validationFlag,
		branchCheckFlag,
		planFlag,
		skipFlag(calico.CutSteps()),
	}
	branchCutAction = func(cfg *Config) func(ctx context.Context, c *cli.Command) error {
		return func(_ context.Context, c *cli.Command) error {
			configureLogging("branch-cut.log")

			calicoManager, err := calico.NewManager(append(
				managerOptions(cfg, c),
				calico.WithMainBranch(c.String(mainBranchFlag.Name)),
				calico.WithReleaseBranchValidation(c.Bool(branchCheckFlag.Name)),
			)...)
			if err != nil {
				return fmt.Errorf("calico manager: %w", err)
			}

			return calicoManager.CutBranch()
		}
	}
)

func branchCutCommand(cfg *Config) *cli.Command {
	return &cli.Command{
		Name:   "cut",
		Usage:  fmt.Sprintf("Cut a new release branch from %s", defaults.Branch),
		Flags:  branchCutFlags,
		Action: branchCutAction(cfg),
	}
}
