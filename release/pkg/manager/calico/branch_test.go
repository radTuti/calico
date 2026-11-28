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

package calico

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/projectcalico/calico/release/internal/branch"
	"github.com/projectcalico/calico/release/internal/github"
)

// TestCut asserts a cut off main emits no derived tag and
// advances main to the next minor.
func TestCut(t *testing.T) {
	f := newFakeRunner().on("git describe --tags --match v[0-9]*.[0-9]*.[0-9]* --abbrev=0 master", "v3.33.0-0.dev", nil)
	m := &CalicoManager{runner: f}
	root := t.TempDir()
	for _, opt := range []Option{
		WithRepoRoot(root),
		WithMainBranch("master"),
		WithReleaseBranchPrefix("release"),
		WithDevTagIdentifier("0.dev"),
	} {
		if err := opt(m); err != nil {
			t.Fatal(err)
		}
	}

	plan, err := m.cut()
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff("release-v3.33", plan.Derived); diff != "" {
		t.Fatalf("plan.Derived (-want +got):\n%s", diff)
	}

	var derivedTag, mainTag string
	for _, tt := range plan.TagTargets {
		switch tt.Branch {
		case "release-v3.33":
			derivedTag = tt.DevTag
		case "master":
			mainTag = tt.DevTag
		}
	}
	if len(derivedTag) > 0 {
		t.Fatalf("the derived branch inherits main's tag, so no new derived tag = %v, want empty", derivedTag)
	}
	if diff := cmp.Diff("v3.34.0-0.dev", mainTag); diff != "" {
		t.Fatalf("main advances to the next minor (-want +got):\n%s", diff)
	}

	t.Run("a resume after main was tagged cuts the version before the tag", func(t *testing.T) {
		f := newFakeRunner().
			on("git describe --tags --match v[0-9]*.[0-9]*.[0-9]* --abbrev=0 master", "v3.34.0-0.dev", nil).
			on("git log -1 --format=%s master", "Begin development for v3.34.0-0.dev\n", nil).
			on("git describe --tags --match v[0-9]*.[0-9]*.[0-9]* --abbrev=0 master^", "v3.33.0-0.dev", nil)
		m := &CalicoManager{runner: f, repoRoot: root, mainBranch: "master", releaseBranchPrefix: "release", devTagIdentifier: "0.dev"}
		plan, err := m.cut()
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff("release-v3.33", plan.Derived); diff != "" {
			t.Fatalf("plan.Derived (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]branch.TagTarget{{Branch: "master", DevTag: "v3.34.0-0.dev"}}, plan.TagTargets); diff != "" {
			t.Fatalf("plan.TagTargets (-want +got):\n%s", diff)
		}
	})

	t.Run("rejects an empty dev tag identifier or branch prefix before reading git", func(t *testing.T) {
		f := newFakeRunner()
		m := &CalicoManager{runner: f, repoRoot: t.TempDir(), mainBranch: "master"}
		_, err := m.cut()
		if err == nil || !strings.Contains(err.Error(), "dev tag identifier") || !strings.Contains(err.Error(), "release branch prefix") {
			t.Errorf("cut() = %v, want both missing values named", err)
		}
		if len(f.calls()) > 0 {
			t.Errorf("ran %v before rejecting the options", f.calls())
		}
	})
}

func TestPrepareDerived(t *testing.T) {
	t.Run("rejects a branch that is not a release branch", func(t *testing.T) {
		f := newFakeRunner()
		r := &CalicoManager{runner: f, repoRoot: t.TempDir(), releaseBranchPrefix: "release"}
		if _, err := r.prepareDerived("master"); err == nil {
			t.Fatal("want an error")
		}
		if len(f.calls()) > 0 {
			t.Errorf("ran %v before rejecting the branch", f.calls())
		}
	})
}

func TestPrepareRelease(t *testing.T) {
	t.Run("beforePrep runs on the base branch and its error stops the prep", func(t *testing.T) {
		f := newFakeRunner()
		f.on("git rev-parse --abbrev-ref HEAD", "release-v3.30\n", nil)
		r := &CalicoManager{runner: f, repoRoot: t.TempDir(), repo: github.Repo{Owner: "projectcalico", Name: "calico", Remote: "origin"}, calicoVersion: testVersion(t, "v3.30.0"), releaseBranchPrefix: "release"}
		r.beforePrep.Do = func() error {
			if f.ran("git switch") {
				t.Errorf("switched branch before beforePrep (calls: %v)", f.calls())
			}
			return os.ErrInvalid
		}
		if err := r.PrepareRelease(); !errors.Is(err, os.ErrInvalid) {
			t.Fatalf("PrepareRelease() = %v, want beforePrep's error", err)
		}
		if f.ran("git switch") {
			t.Errorf("switched branch after beforePrep failed (calls: %v)", f.calls())
		}
	})

}

func TestPrepSpec(t *testing.T) {
	t.Run("records the base branch and stages only existing paths", func(t *testing.T) {
		f := newFakeRunner()
		root := t.TempDir()
		values, err := filepath.Glob("../../../../charts/*/values.yaml")
		if err != nil || len(values) == 0 {
			t.Fatalf("find chart values: %v", err)
		}
		for _, src := range values {
			b, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			dst := filepath.Join(root, "charts", filepath.Base(filepath.Dir(src)), "values.yaml")
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		r := &CalicoManager{
			runner: f, repoRoot: root, repo: github.Repo{Owner: "projectcalico", Name: "calico", Remote: "origin"}, calicoVersion: testVersion(t, "v3.30.0"),
			releaseBranchPrefix: "release", imageRegistries: []string{"quay.io/calico"},
			operatorImage: "tigera/operator", operatorVersion: "v1.38.0", operatorRegistry: "quay.io",
		}
		spec := r.prepSpec("release-v3.30")
		if diff := cmp.Diff("build: v3.30.0 release", spec.CommitMessage); diff != "" {
			t.Errorf("commit message (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"Release-branch: release-v3.30"}, spec.Trailers); diff != "" {
			t.Errorf("trailers (-want +got):\n%s", diff)
		}
		paths, err := spec.Edit()
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff([]string{filepath.Join(root, "charts")}, paths); diff != "" {
			t.Errorf("staged paths (-want +got):\n%s", diff)
		}
	})
}

func TestPrepPrereqs(t *testing.T) {
	for _, tc := range []struct {
		name           string
		remote         string
		validateBranch bool
		wantErr        bool
		wantWarn       bool
	}{
		{name: "main repo", remote: "git@github.com:projectcalico/calico.git", validateBranch: true},
		{name: "fork", remote: "git@github.com:someone/calico.git", validateBranch: true, wantErr: true},
		{name: "fork with the branch check off warns", remote: "git@github.com:someone/calico.git", wantWarn: true},
		{name: "main repo with the branch check off warns", remote: "git@github.com:projectcalico/calico.git", wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRunner()
			f.on("git rev-parse --abbrev-ref HEAD", "release-v3.30\n", nil)
			f.on("git config --get remote.origin.url", tc.remote+"\n", nil)
			r := &CalicoManager{
				runner: f, repoRoot: "/repo", repo: github.Repo{Owner: "projectcalico", Name: "calico", Remote: "origin"},
				calicoVersion: testVersion(t, "v3.30.0"), operatorVersion: "v1.38.0",
				releaseBranchPrefix: "release", validate: true, validateBranch: tc.validateBranch,
				gitRef: true,
			}
			hook := logtest.NewGlobal()
			t.Cleanup(hook.Reset)
			err := r.prepPrereqs()
			if got := err != nil && strings.Contains(err.Error(), "expected to be from"); got != tc.wantErr {
				t.Errorf("prepPrereqs() = %v, want fork error %v", err, tc.wantErr)
			}
			var warned bool
			for _, e := range hook.AllEntries() {
				warned = warned || e.Message == "Skipping branch check because validateBranch is false"
			}
			if warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v", warned, tc.wantWarn)
			}
		})
	}

	t.Run("a missing GitHub repo fails with validation off", func(t *testing.T) {
		r := &CalicoManager{runner: newFakeRunner(), repoRoot: "/repo", repo: github.Repo{Remote: "origin"}}
		if err := r.prepPrereqs(); err == nil {
			t.Error("prepPrereqs() = nil, want an error")
		}
	})
}
