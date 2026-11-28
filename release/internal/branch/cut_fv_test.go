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

// localCutSteps are the cut, edit, and tag steps in order, without push-refs
// and post-push (which need a remote).
var localCutSteps = []string{stepCutBranch, stepUpdateDerived, stepTagDerived, stepUpdateMain, stepTagMain}

func TestCutNodes(t *testing.T) {
	// The head is the edits commit, not an empty "Begin development" commit.
	t.Run("lands the edit on the new branch", func(t *testing.T) {
		root := initGitRepo(t)
		derived := testPrefix + "-v3.33"
		p := Cut{
			Main:    "master",
			Derived: derived,
			Source:  "master",
			PrepareDerived: func(string) ([]string, error) {
				written, err := branchEditPrepare(t, root, derived)("")
				writeFile(t, root, "hook-file.txt", "written by the hook\n")
				return append(written, "hook-file.txt"), err
			},
			TagTargets: []TagTarget{{Branch: derived, DevTag: "v3.33.0-" + testSuffix}},
		}
		d, g := stepDriver(root, nil, plan.Options{})
		if err := runNodes(t, d, g, p, localCutSteps...); err != nil {
			t.Fatal(err)
		}

		meta, err := realGit(root).Run("show", derived+":metadata.mk")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(meta, "MANAGER_BRANCH ?= "+derived) {
			t.Fatalf("metadata.mk on %s = %q, want the MANAGER_BRANCH edit", derived, meta)
		}
		hook, err := realGit(root).Run("show", derived+":hook-file.txt")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff("written by the hook", hook); diff != "" {
			t.Fatalf("hook-file.txt (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff("Updates for "+derived+" release branch", gitHeadSubject(t, root, derived)); diff != "" {
			t.Fatalf("head subject (-want +got):\n%s", diff)
		}
	})

	// PrepareMain bumps CALICO_VERSION on main, not the new branch; main's dev tag
	// sits on main's head.
	t.Run("bumps and tags main", func(t *testing.T) {
		root := srcBumpRepo(t)
		var ranOn string

		p := Cut{
			Main:    "master",
			Derived: testPrefix + "-v3.33-1",
			Source:  "master",
			TagTargets: []TagTarget{
				{Branch: testPrefix + "-v3.33-1", DevTag: "v3.33.0-1.0-" + testSuffix},
				{Branch: "master", DevTag: "v3.34.0-1.0-" + testSuffix},
			},
		}
		d, g := stepDriver(root, nil, plan.Options{})
		p.PrepareDerived = branchEditPrepare(t, root, testPrefix+"-v3.33-1")
		p.PrepareMain = bumpMainPrepare(t, root, &ranOn)

		if err := runNodes(t, d, g, p, localCutSteps...); err != nil {
			t.Fatal(err)
		}

		// PrepareMain ran on main, not the new branch.
		if diff := cmp.Diff("master", ranOn); diff != "" {
			t.Fatalf("ranOn mismatch (-want +got):\n%s", diff)
		}

		// master's CALICO_VERSION is bumped; the new branch's is not.
		masterMeta, err := realGit(root).Run("show", "master:metadata.mk")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(masterMeta, "CALICO_VERSION=v3.34.0") {
			t.Fatalf("%q missing %q", masterMeta, "CALICO_VERSION=v3.34.0")
		}

		newMeta, err := realGit(root).Run("show", testPrefix+"-v3.33-1:metadata.mk")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(newMeta, "CALICO_VERSION=v3.33.0") {
			t.Fatalf("the main bump must not land on the new branch: %q missing %q", newMeta, "CALICO_VERSION=v3.33.0")
		}
		if !strings.Contains(newMeta, "MANAGER_BRANCH ?= "+testPrefix+"-v3.33-1") {
			t.Fatalf("the new-branch edit must land on the new branch: %q missing %q", newMeta, "MANAGER_BRANCH ?= "+testPrefix+"-v3.33-1")
		}

		// master's dev tag sits on master's head.
		tagged, err := realGit(root).Run("rev-parse", "v3.34.0-1.0-"+testSuffix+"^{commit}")
		if err != nil {
			t.Fatal(err)
		}
		head, err := realGit(root).Run("rev-parse", "master")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(strings.TrimSpace(head), strings.TrimSpace(tagged)); diff != "" {
			t.Fatalf("main tag commit (-want +got):\n%s", diff)
		}
	})

	// Skipping update-main leaves main's pin unbumped, while tag-main still tags
	// master's head with an empty commit.
	t.Run("skipping update-main leaves the pin unbumped", func(t *testing.T) {
		root := srcBumpRepo(t)
		var ranOn string

		p := Cut{
			Main:    "master",
			Derived: testPrefix + "-v3.33-1",
			Source:  "master",
			TagTargets: []TagTarget{
				{Branch: testPrefix + "-v3.33-1", DevTag: "v3.33.0-1.0-" + testSuffix},
				{Branch: "master", DevTag: "v3.34.0-1.0-" + testSuffix},
			},
		}
		d, g := stepDriver(root, nil, plan.Options{Skip: map[string]bool{stepUpdateMain: true}})
		p.PrepareDerived = branchEditPrepare(t, root, testPrefix+"-v3.33-1")
		p.PrepareMain = bumpMainPrepare(t, root, &ranOn)

		if err := runNodes(t, d, g, p, localCutSteps...); err != nil {
			t.Fatal(err)
		}

		// update-main was skipped, so PrepareMain never ran and the pin is unbumped.
		if len(ranOn) != 0 {
			t.Fatalf("update-main must not run when skipped: %q", ranOn)
		}
		masterMeta, err := realGit(root).Run("show", "master:metadata.mk")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(masterMeta, "CALICO_VERSION=v3.33.0") {
			t.Fatalf("skipping update-main must leave main's pin unbumped: %q missing %q", masterMeta, "CALICO_VERSION=v3.33.0")
		}

		// tag-main still tags master's head.
		tagged, err := realGit(root).Run("rev-parse", "v3.34.0-1.0-"+testSuffix+"^{commit}")
		if err != nil {
			t.Fatal(err)
		}
		head, err := realGit(root).Run("rev-parse", "master")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(strings.TrimSpace(head), strings.TrimSpace(tagged)); diff != "" {
			t.Fatalf("main tag commit (-want +got):\n%s", diff)
		}
	})

	// With no main tag target, update-main and tag-main no-op even when a
	// PrepareMain hook is set.
	t.Run("no main target skips the main steps", func(t *testing.T) {
		root := srcBumpRepo(t)
		var ranOn string

		p := Cut{
			Main:    "master",
			Derived: testPrefix + "-v3.33-1",
			Source:  "master",
			TagTargets: []TagTarget{
				{Branch: testPrefix + "-v3.33-1", DevTag: "v3.33.0-1.0-" + testSuffix},
			},
		}
		d, g := stepDriver(root, nil, plan.Options{})
		p.PrepareDerived = branchEditPrepare(t, root, testPrefix+"-v3.33-1")
		p.PrepareMain = bumpMainPrepare(t, root, &ranOn)

		if err := runNodes(t, d, g, p, localCutSteps...); err != nil {
			t.Fatal(err)
		}

		// No main target: PrepareMain must not run and main is untouched.
		if len(ranOn) != 0 {
			t.Fatalf("no main target must not run PrepareMain: %q", ranOn)
		}
		masterMeta, err := realGit(root).Run("show", "master:metadata.mk")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(masterMeta, "CALICO_VERSION=v3.33.0") {
			t.Fatalf("no main target must leave the pin unbumped: %q missing %q", masterMeta, "CALICO_VERSION=v3.33.0")
		}

		// No main dev tag is created.
		_, err = realGit(root).Run("rev-parse", "--verify", "--quiet", "refs/tags/v3.34.0-1.0-"+testSuffix)
		if err == nil {
			t.Fatalf("no main target must not create a main dev tag")
		}
	})

	refs := func(t *testing.T, dirs ...string) []string {
		t.Helper()
		var out []string
		for _, dir := range dirs {
			s, err := realGit(dir).Run("for-each-ref", "--format=%(refname) %(objectname)")
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, strings.Split(strings.TrimSpace(s), "\n")...)
		}
		return out
	}
	mainCut := func(t *testing.T, root string, prepareDerived func(string) ([]string, error)) Cut {
		var ranOn string
		return Cut{
			Main:           "master",
			Source:         "master",
			Derived:        testPrefix + "-v3.33",
			Validate:       true,
			PrepareDerived: prepareDerived,
			PrepareMain:    bumpMainPrepare(t, root, &ranOn),
			TagTargets:     []TagTarget{{Branch: "master", DevTag: "v3.34.0-" + testSuffix}},
		}
	}
	trackedRepo := func(t *testing.T) (root, remote string) {
		root = srcBumpRepo(t)
		remote = initGitRemote(t, root)
		if _, err := realGit(root).Run("branch", "--set-upstream-to=origin/master", "master"); err != nil {
			t.Fatal(err)
		}
		return root, remote
	}

	t.Run("a re-run of a finished cut changes nothing", func(t *testing.T) {
		root, remote := trackedRepo(t)
		c := mainCut(t, root, branchEditPrepare(t, root, testPrefix+"-v3.33"))
		d := newTestDriver(t, root, WithPublish(true))
		if err := runCut(d, c); err != nil {
			t.Fatal(err)
		}
		before := refs(t, root, remote)
		if err := runCut(d, c); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(before, refs(t, root, remote)); diff != "" {
			t.Fatalf("refs after the re-run (-want +got):\n%s", diff)
		}
	})

	t.Run("a re-run finishes a failed cut", func(t *testing.T) {
		root, remote := trackedRepo(t)
		derived := testPrefix + "-v3.33"
		edit := branchEditPrepare(t, root, derived)
		d := newTestDriver(t, root, WithPublish(true))
		failing := mainCut(t, root, func(b string) ([]string, error) {
			if _, err := edit(b); err != nil {
				t.Fatal(err)
			}
			return nil, errors.New("hook failed")
		})
		if err := runCut(d, failing); err == nil || !strings.Contains(err.Error(), "hook failed") {
			t.Fatalf("err = %v, want it to contain %q", err, "hook failed")
		}
		cur, err := realGit(root).Branch()
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff("master", cur); diff != "" {
			t.Fatalf("branch after the failure (-want +got):\n%s", diff)
		}

		if err := runCut(d, mainCut(t, root, edit)); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff("Updates for "+derived+" release branch", gitHeadSubject(t, root, derived)); diff != "" {
			t.Fatalf("head subject of %s (-want +got):\n%s", derived, diff)
		}
		pushed, err := realGit(remote).Resolve("refs/heads/" + derived)
		if err != nil {
			t.Fatal(err)
		}
		local, err := realGit(root).Resolve("refs/heads/" + derived)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(local, pushed); diff != "" {
			t.Fatalf("pushed %s (-want +got):\n%s", derived, diff)
		}
	})

	// A cut off a non-main branch runs on that branch and ends back on it.
	t.Run("a cut off another branch starts and ends on it", func(t *testing.T) {
		root := initGitRepo(t)
		initGitRemote(t, root)
		g := realGit(root)
		for _, args := range [][]string{
			{"branch", "--set-upstream-to=origin/master", "master"},
			{"switch", "-q", "-c", "ep-1"},
			{"push", "-q", "origin", "ep-1"},
		} {
			if _, err := g.Run(args...); err != nil {
				t.Fatalf("git %v: %v", args, err)
			}
		}
		derived := testPrefix + "-v3.33-ep"
		c := Cut{Main: "master", Source: "ep-1", Derived: derived, Validate: true, PrepareDerived: branchEditPrepare(t, root, derived)}
		if err := runCut(newTestDriver(t, root), c); err != nil {
			t.Fatal(err)
		}
		if !branchExists(t, g, derived) {
			t.Fatalf("branch %s was not cut", derived)
		}
		cur, err := g.Branch()
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff("ep-1", cur); diff != "" {
			t.Fatalf("current branch (-want +got):\n%s", diff)
		}
	})

	// An unrelated pre-staged file left in the index must not enter the cut commit.
	t.Run("commits only the given paths", func(t *testing.T) {
		root := initGitRepo(t)
		g := realGit(root)

		writeFile(t, root, "wanted.txt", "wanted\n")
		writeFile(t, root, "unrelated.txt", "unrelated\n")
		_, err := g.Run("add", "unrelated.txt")
		if err != nil {
			t.Fatal(err)
		}

		committed, err := stageAndCommit(g, "cut edit", nil, []string{"wanted.txt"})
		if err != nil {
			t.Fatal(err)
		}
		if !committed {
			t.Fatal("committed = false")
		}

		files, err := realGit(root).Run("show", "--name-only", "--format=", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(files, "wanted.txt") {
			t.Fatalf("%q missing %q", files, "wanted.txt")
		}
		if strings.Contains(files, "unrelated.txt") {
			t.Fatalf("an unrelated pre-staged file must not enter the cut commit: %q has %q", files, "unrelated.txt")
		}
	})

	// A run that failed before the edit landed resumes from the advanced source.
	t.Run("resets when the source advanced", func(t *testing.T) {
		root := initGitRepo(t)
		derived := testPrefix + "-v3.33"
		p := Cut{Main: "master", Derived: derived, Source: "master", PrepareDerived: branchEditPrepare(t, root, derived)}
		d, g := stepDriver(root, nil, plan.Options{})
		if err := runNodes(t, d, g, p, stepCutBranch); err != nil {
			t.Fatal(err)
		}

		_, err := realGit(root).Run("checkout", "-q", "master")
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, root, "new-on-master.txt", "added after the cut\n")
		_, err = realGit(root).Run("add", ".")
		if err != nil {
			t.Fatal(err)
		}
		_, err = realGit(root).Run("commit", "-q", "-m", "advance master")
		if err != nil {
			t.Fatal(err)
		}

		if err := runNodes(t, d, g, p, stepUpdateDerived); err != nil {
			t.Fatal(err)
		}

		content, err := realGit(root).Run("show", derived+":new-on-master.txt")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(content, "added after the cut") {
			t.Fatalf("derived must pick up the advanced source commit: %q missing %q", content, "added after the cut")
		}
		meta, err := realGit(root).Run("show", derived+":metadata.mk")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(meta, "MANAGER_BRANCH ?= "+derived) {
			t.Fatalf("the edit must be re-applied after the reset: %q missing %q", meta, "MANAGER_BRANCH ?= "+derived)
		}
	})

	// A stale local derived tag reads as not-done and tag-derived
	// force-moves it onto the branch head.
	t.Run("moves a stale local tag", func(t *testing.T) {
		root := initGitRepo(t)

		p := Cut{
			Main:    "master",
			Derived: testPrefix + "-v3.33",
			Source:  "master",
			TagTargets: []TagTarget{
				{Branch: testPrefix + "-v3.33", DevTag: "v3.33.0-" + testSuffix},
			},
		}
		d, g := stepDriver(root, nil, plan.Options{})
		p.PrepareDerived = func(string) ([]string, error) {
			written, _, err := ApplyEdits(root, []Edit{
				{File: "metadata.mk", Pattern: `^MANAGER_BRANCH.*`, Replacement: "MANAGER_BRANCH ?= " + testPrefix + "-v3.33"},
			})
			return written, err
		}

		// Create the stale tag on master, at a commit that is not the derived head.
		_, err := realGit(root).Run("tag", "--no-sign", "v3.33.0-"+testSuffix)
		if err != nil {
			t.Fatal(err)
		}
		staleSha, err := realGit(root).Run("rev-parse", "v3.33.0-"+testSuffix+"^{commit}")
		if err != nil {
			t.Fatal(err)
		}

		all := cutNodes(d, g, p)
		cut := findNode(t, all, stepCutBranch)
		update := findNode(t, all, stepUpdateDerived)
		tagDerived := findNode(t, all, stepTagDerived)

		if err := cut.Do(); err != nil {
			t.Fatal(err)
		}
		if err := update.Do(); err != nil {
			t.Fatal(err)
		}

		if err := tagDerived.Done(); err == nil {
			t.Fatalf("a local tag at the wrong commit must not read as done")
		}

		if err := tagDerived.Do(); err != nil {
			t.Fatal(err)
		}

		head, err := realGit(root).Run("rev-parse", testPrefix+"-v3.33")
		if err != nil {
			t.Fatal(err)
		}
		movedSha, err := realGit(root).Run("rev-parse", "v3.33.0-"+testSuffix+"^{commit}")
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(strings.TrimSpace(head), strings.TrimSpace(movedSha)); diff != "" {
			t.Fatalf("the tag must move onto the derived head (-want +got):\n%s", diff)
		}
		if cmp.Equal(strings.TrimSpace(staleSha), strings.TrimSpace(movedSha)) {
			t.Fatalf("the tag must have actually moved: both %q", strings.TrimSpace(staleSha))
		}

		if err := tagDerived.Done(); err != nil {
			t.Fatalf("once the tag sits on the derived head, the step must read as done: %v", err)
		}
	})
}

// srcBumpRepo makes a temp repo whose metadata.mk carries CALICO_VERSION v3.33.0.
func srcBumpRepo(t *testing.T) string {
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
	writeFile(t, root, "metadata.mk", "MANAGER_BRANCH ?= master\nCALICO_VERSION=v3.33.0\n")
	run("add", ".")
	run("commit", "-q", "-m", "initial")
	return root
}

// branchEditPrepare returns a PrepareDerived hook that bumps MANAGER_BRANCH,
// a stand-in for the real derived-branch edit.
func branchEditPrepare(t *testing.T, root, derived string) func(string) ([]string, error) {
	t.Helper()
	return func(string) ([]string, error) {
		written, skipped, err := ApplyEdits(root, []Edit{
			{File: "metadata.mk", Pattern: `^MANAGER_BRANCH.*`, Replacement: "MANAGER_BRANCH ?= " + derived},
		})
		if len(skipped) != 0 {
			t.Fatalf("skipped = %q, want empty", skipped)
		}
		return written, err
	}
}

// bumpMainPrepare returns a PrepareMain hook that bumps CALICO_VERSION on the
// main branch and records the branch it ran on.
func bumpMainPrepare(t *testing.T, root string, ranOn *string) func(string) ([]string, error) {
	t.Helper()
	return func(mainBranch string) ([]string, error) {
		*ranOn = mainBranch
		written, skipped, err := ApplyEdits(root, []Edit{
			{File: "metadata.mk", Pattern: `^CALICO_VERSION=.*`, Replacement: "CALICO_VERSION=v3.34.0"},
		})
		if len(skipped) != 0 {
			t.Fatalf("skipped = %q, want empty", skipped)
		}
		return written, err
	}
}

func TestValidateFreshCut(t *testing.T) {
	// Validate off skips the dirty-tree reject, so a dirty tree does not error.
	t.Run("without validate a dirty tree passes", func(t *testing.T) {
		root := initGitRepo(t)
		initGitRemote(t, root)
		_, err := realGit(root).Run("branch", "--set-upstream-to=origin/master", "master")
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, root, "metadata.mk", "MANAGER_BRANCH ?= dirty\n")
		d := newTestDriver(t, root)
		p := Cut{Main: "master", Derived: testPrefix + "-v3.33", Source: "master"}
		if err := d.validateFreshCut(realGit(root), p); err != nil {
			t.Fatal(err)
		}
	})

	// A fresh cut with Validate rejects a dirty tree; a resume does not, and plan
	// mode only reports it.
	t.Run("rejects a dirty tree", func(t *testing.T) {
		root := initGitRepo(t)
		// master is current with its upstream so the base check passes and only the
		// dirty-tree gate is under test.
		initGitRemote(t, root)
		_, err := realGit(root).Run("branch", "--set-upstream-to=origin/master", "master")
		if err != nil {
			t.Fatal(err)
		}
		// Make the tracked tree dirty.
		writeFile(t, root, "metadata.mk", "MANAGER_BRANCH ?= dirty\n")

		g := realGit(root)
		p := Cut{Main: "master", Derived: testPrefix + "-v3.33", Source: "master", Validate: true}

		d := newTestDriver(t, root)

		// fresh cut (derived branch absent) + dirty -> rejected.
		err = d.validateFreshCut(g, p)
		if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
			t.Fatalf("error = %v, want one containing %q", err, "uncommitted changes")
		}

		// plan mode reports the dirty tree instead of failing.
		planD := newTestDriver(t, root, WithPlanOptions(plan.Options{Preview: true}))
		if err := runCut(planD, p); err != nil {
			t.Fatal(err)
		}

		// --skip is not a resume signal: a fresh cut with --skip still validates.
		skipD := newTestDriver(t, root, WithPlanOptions(plan.Options{Skip: map[string]bool{"cut-branch": true}}))
		err = skipD.validateFreshCut(g, p)
		if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
			t.Fatalf("error = %v, want one containing %q", err, "uncommitted changes")
		}

		// resume: derived branch exists -> not validated even when dirty.
		_, err = realGit(root).Run("branch", p.Derived)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.validateFreshCut(g, p); err != nil {
			t.Fatal(err)
		}
	})

	// A fresh cut must start on main; a resume may run from anywhere.
	t.Run("a fresh cut must run on main", func(t *testing.T) {
		root := initGitRepo(t)
		initGitRemote(t, root)
		g := realGit(root)
		for _, args := range [][]string{
			{"branch", "--set-upstream-to=origin/master", "master"},
			{"switch", "-q", "-c", "some-feature"},
		} {
			if _, err := g.Run(args...); err != nil {
				t.Fatal(err)
			}
		}
		d := newTestDriver(t, root)
		p := Cut{Main: "master", Derived: testPrefix + "-v3.33", Source: "master", Validate: true}

		err := d.validateFreshCut(g, p)
		if err == nil || !strings.Contains(err.Error(), "must run on master") {
			t.Fatalf("error = %v, want one containing %q", err, "must run on master")
		}

		if _, err := g.Run("branch", p.Derived, "master"); err != nil {
			t.Fatal(err)
		}
		if err := d.validateFreshCut(g, p); err != nil {
			t.Fatal(err)
		}
	})

	// Checks the base check runs even
	// with Validate off: a main behind its upstream is still rejected.
	t.Run("checks the base without validate", func(t *testing.T) {
		root := initGitRepo(t)
		initGitRemote(t, root)
		run := func(args ...string) {
			_, err := realGit(root).Run(args...)
			if err != nil {
				t.Fatalf("git %v: %v", args, err)
			}
		}
		// Push an extra commit to origin, then reset local master back so it is behind
		// its upstream.
		run("branch", "--set-upstream-to=origin/master", "master")
		run("commit", "-q", "--allow-empty", "-m", "ahead")
		run("push", "-q", "origin", "master")
		run("reset", "-q", "--hard", "HEAD~1")

		d := newTestDriver(t, root)
		p := Cut{Main: "master", Derived: testPrefix + "-v3.33", Source: "master"}
		err := d.validateFreshCut(realGit(root), p)
		if err == nil || !strings.Contains(err.Error(), "not in sync") {
			t.Fatalf("error = %v, want one containing %q", err, "not in sync")
		}
	})
}
