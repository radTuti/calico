// Copyright (c) 2026 Tigera, Inc. All rights reserved.

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

//go:build fv

package branch

import (
	"os"
	"strings"
	"testing"

	"github.com/projectcalico/calico/release/internal/command"
)

// A worktree build exports GIT_DIR, which would point every temp repo at the
// real checkout.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("GIT_DIR")
	_ = os.Unsetenv("GIT_WORK_TREE")
	os.Exit(m.Run())
}

func realGit(root string) command.GitRunner {
	return command.Git(command.RealRunner).In(root)
}

func branchExists(t *testing.T, g command.GitRunner, name string) bool {
	t.Helper()
	exists, err := localBranchExists(g, name)
	if err != nil {
		t.Fatal(err)
	}
	return exists
}

// initGitRepo makes a temp repo with one commit on "master" and returns its root.
func initGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	run := func(args ...string) {
		_, err := realGit(root).Run(args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	run("init", "-q", "-b", "master")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	run("config", "commit.gpgsign", "false")
	writeFile(t, root, "metadata.mk", "MANAGER_BRANCH ?= master\n")
	run("add", ".")
	run("commit", "-q", "-m", "initial")
	return root
}

func gitHeadSubject(t *testing.T, root, ref string) string {
	t.Helper()
	out, err := realGit(root).Run("log", "-1", "--format=%s", ref)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

// initGitRemote makes a bare repo to serve as root's "origin", pushes master to
// it, and returns the remote path.
func initGitRemote(t *testing.T, root string) string {
	t.Helper()
	remote := t.TempDir()
	run := func(dir string, args ...string) string {
		out, err := realGit(dir).Run(args...)
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return out
	}
	run(remote, "init", "-q", "--bare", "-b", "master")
	run(remote, "config", "tag.gpgsign", "false")
	run(root, "remote", "add", "origin", remote)
	run(root, "push", "-q", "origin", "master")
	return remote
}
