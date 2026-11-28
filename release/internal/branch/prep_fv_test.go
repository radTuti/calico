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
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/projectcalico/calico/release/internal/plan"
)

func TestPrepNodes(t *testing.T) {
	const prepBranch = "build-v3.33.1"

	// newPrep returns a prep off master whose edit writes version.txt, and
	// records the branch the edit ran on.
	newPrep := func(t *testing.T, root string, ran *[]string) Prep {
		return Prep{
			Base:          "master",
			Branch:        prepBranch,
			CommitMessage: "build: v3.33.1 release",
			Trailers:      []string{"Release-Branch: master"},
			Edit: func() ([]string, error) {
				cur, err := realGit(root).Branch()
				if err != nil {
					t.Fatal(err)
				}
				*ran = append(*ran, "edit@"+cur)
				writeFile(t, root, "version.txt", "v3.33.1\n")
				return []string{"version.txt"}, nil
			},
		}
	}
	run := func(d *Driver, p Prep) error {
		nodes, err := d.Prep(p)
		if err != nil {
			return err
		}
		pl, err := plan.New(d.opts, nodes...)
		if err != nil {
			return err
		}
		return pl.Run()
	}
	currentBranch := func(t *testing.T, root string) string {
		cur, err := realGit(root).Branch()
		if err != nil {
			t.Fatal(err)
		}
		return cur
	}
	commitsAhead := func(t *testing.T, root string) string {
		out, err := realGit(root).Run("rev-list", "--count", "master.."+prepBranch)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out)
	}

	// A file a caller's node wrote on the base carries over into the commit;
	// the commit carries the trailer; the run ends back on the base.
	t.Run("commits the edit and returns to the base", func(t *testing.T) {
		root := initGitRepo(t)
		writeFile(t, root, "notes.md", "notes\n")
		var ran []string
		p := newPrep(t, root, &ran)
		edit := p.Edit
		p.Edit = func() ([]string, error) {
			paths, err := edit()
			return append(paths, "notes.md"), err
		}
		if err := run(newTestDriver(t, root), p); err != nil {
			t.Fatal(err)
		}

		if diff := cmp.Diff([]string{"edit@" + prepBranch}, ran); diff != "" {
			t.Errorf("edit runs (-want +got):\n%s", diff)
		}
		files, err := realGit(root).Run("show", "--name-only", "--format=", prepBranch)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff([]string{"notes.md", "version.txt"}, strings.Fields(files)); diff != "" {
			t.Errorf("committed files (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff("master", currentBranch(t, root)); diff != "" {
			t.Fatalf("current branch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff("build: v3.33.1 release", gitHeadSubject(t, root, prepBranch)); diff != "" {
			t.Fatalf("prep commit subject (-want +got):\n%s", diff)
		}
		trailer, err := realGit(root).Run("log", "-1", "--format=%(trailers:key=Release-Branch,valueonly)", prepBranch)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff("master", strings.TrimSpace(trailer)); diff != "" {
			t.Fatalf("Release-Branch trailer (-want +got):\n%s", diff)
		}
	})

	// A re-run resets the prep branch to the base, so it never stacks commits.
	t.Run("a re-run starts the prep branch over", func(t *testing.T) {
		root := initGitRepo(t)
		var ran []string
		d := newTestDriver(t, root)
		if err := run(d, newPrep(t, root, &ran)); err != nil {
			t.Fatal(err)
		}
		if err := run(d, newPrep(t, root, &ran)); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff("1", commitsAhead(t, root)); diff != "" {
			t.Fatalf("prep commits ahead of master (-want +got):\n%s", diff)
		}
	})

	t.Run("a failed edit switches back to the base and discards the edit", func(t *testing.T) {
		root := initGitRepo(t)
		before := readFile(t, root, "metadata.mk")
		var ran []string
		p := newPrep(t, root, &ran)
		p.Edit = func() ([]string, error) {
			writeFile(t, root, "metadata.mk", "half-done\n")
			return nil, errors.New("edit failed")
		}
		if err := run(newTestDriver(t, root), p); err == nil || !strings.Contains(err.Error(), "edit failed") {
			t.Fatalf("err = %v, want it to contain %q", err, "edit failed")
		}
		if diff := cmp.Diff("master", currentBranch(t, root)); diff != "" {
			t.Fatalf("current branch (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff(before, readFile(t, root, "metadata.mk")); diff != "" {
			t.Fatalf("metadata.mk (-want +got):\n%s", diff)
		}
	})

	// An edit that changes nothing leaves nothing to deliver, so the run fails
	// before it pushes.
	t.Run("an edit with no changes fails", func(t *testing.T) {
		root := initGitRepo(t)
		remote := initGitRemote(t, root)
		var ran []string
		p := newPrep(t, root, &ran)
		p.Edit = func() ([]string, error) { return []string{"metadata.mk"}, nil }
		err := run(newTestDriver(t, root, WithPublish(true)), p)
		if err == nil || !strings.Contains(err.Error(), "commit on "+prepBranch+" since master") {
			t.Fatalf("err = %v, want it to contain %q", err, "commit on "+prepBranch+" since master")
		}
		if branchExists(t, realGit(remote), prepBranch) {
			t.Fatalf("remote has %s, want it absent", prepBranch)
		}
	})

	// A re-run force-pushes the reset branch over the old one.
	t.Run("publishing pushes the prep branch", func(t *testing.T) {
		root := initGitRepo(t)
		remote := initGitRemote(t, root)
		var ran []string
		d := newTestDriver(t, root, WithPublish(true))
		if err := run(d, newPrep(t, root, &ran)); err != nil {
			t.Fatal(err)
		}

		_, err := realGit(root).Run("commit", "-q", "--allow-empty", "-m", "advance master")
		if err != nil {
			t.Fatal(err)
		}
		if err := run(d, newPrep(t, root, &ran)); err != nil {
			t.Fatal(err)
		}
		local, err := realGit(root).Resolve("refs/heads/" + prepBranch)
		if err != nil {
			t.Fatal(err)
		}
		pushed, err := realGit(remote).Resolve("refs/heads/" + prepBranch)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(local, pushed); diff != "" {
			t.Fatalf("pushed (-want +got):\n%s", diff)
		}
	})

	// A local run pushes with --dry-run, so the remote gets nothing.
	t.Run("a local run writes nothing to the remote", func(t *testing.T) {
		root := initGitRepo(t)
		remote := initGitRemote(t, root)
		var ran []string
		d := newTestDriver(t, root, WithPublish(true), WithPlanOptions(plan.Options{Local: true}))
		if err := run(d, newPrep(t, root, &ran)); err != nil {
			t.Fatal(err)
		}
		if branchExists(t, realGit(remote), prepBranch) {
			t.Fatalf("remote has %s, want it absent", prepBranch)
		}
	})
}
