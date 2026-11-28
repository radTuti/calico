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

package branch

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/projectcalico/calico/release/internal/command"
)

var errFake = fmt.Errorf("fake")

// errNoRef is git's exit status for a ref that names nothing.
var errNoRef = &command.Error{Err: errFake, ExitCode: 1}

// fakeRunner answers each git call from out and errs by key(args). An
// unscripted call returns "" with no error.
func fakeRunner(key func([]string) string, out map[string]string, errs map[string]error) *command.Recorder {
	return &command.Recorder{Reply: func(c command.Call) (string, error) {
		k := key(c.Args)
		return out[k], errs[k]
	}}
}

// firstTwo keys a call by its first two args, so one reply covers every ref.
func firstTwo(args []string) string { return strings.Join(args[:min(2, len(args))], " ") }

func allArgs(args []string) string { return strings.Join(args, " ") }

func gitArgs(r *command.Recorder) [][]string {
	var got [][]string
	for _, c := range r.Calls() {
		got = append(got, c.Args)
	}
	return got
}

func ran(r *command.Recorder, prefix string) bool {
	for _, args := range gitArgs(r) {
		if strings.HasPrefix(strings.Join(args, " "), prefix) {
			return true
		}
	}
	return false
}

func TestLocalBranchExists(t *testing.T) {
	for _, tc := range []struct {
		name    string
		out     string
		err     error
		want    bool
		wantErr bool
	}{
		{name: "a resolved branch exists", out: "abc", want: true},
		{name: "a missing branch does not", err: errNoRef},
		{name: "a git failure is an error", err: &command.Error{Err: errFake, ExitCode: 128}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := fakeRunner(firstTwo, map[string]string{"rev-parse --verify": tc.out}, map[string]error{"rev-parse --verify": tc.err})
			got, err := localBranchExists(command.Git(g), "release-v3.33")
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStageAndCommit(t *testing.T) {
	const diff = "diff --cached --quiet -- a.txt"
	for _, tc := range []struct {
		name    string
		diffErr error
		want    bool
		wantErr bool
	}{
		{name: "unchanged paths skip the commit"},
		{name: "changed paths commit", diffErr: &command.Error{Err: errFake, ExitCode: 1}, want: true},
		{name: "a failed diff is an error", diffErr: &command.Error{Err: errFake, ExitCode: 128}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := fakeRunner(allArgs, nil, map[string]error{diff: tc.diffErr})
			got, err := stageAndCommit(command.Git(g), "msg", []string{"Key: value"}, []string{"a.txt"})
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
			committed := ran(g, "commit -m msg --trailer Key: value -- a.txt")
			if committed != tc.want {
				t.Errorf("ran commit with the trailer = %v, want %v (calls: %v)", committed, tc.want, gitArgs(g))
			}
		})
	}
}

func TestLocalTagPointsAt(t *testing.T) {
	const (
		tagRef    = "rev-parse --verify --quiet refs/tags/v3.33.0"
		branchRef = "rev-parse --verify --quiet refs/heads/release-v3.33"
	)
	for _, tc := range []struct {
		name    string
		out     map[string]string
		errs    map[string]error
		want    bool
		wantErr bool
	}{
		{name: "tag on the branch head", out: map[string]string{tagRef: "abc", branchRef: "abc"}, want: true},
		{name: "tag elsewhere", out: map[string]string{tagRef: "abc", branchRef: "def"}},
		{name: "missing tag", errs: map[string]error{tagRef: errNoRef}},
		{name: "missing branch", out: map[string]string{tagRef: "abc"}, errs: map[string]error{branchRef: errNoRef}},
		{name: "git failure", errs: map[string]error{tagRef: &command.Error{Err: errFake, ExitCode: 128}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := fakeRunner(allArgs, tc.out, tc.errs)
			got, err := localTagPointsAt(command.Git(g), "v3.33.0", "release-v3.33")
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRemoteRefMatches(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"
	ref := testPrefix + "-v3.33-2"

	// present and pointing at the local sha.
	g := fakeRunner(firstTwo, map[string]string{"ls-remote origin": sha + "\trefs/heads/" + ref + "\n"}, nil)
	present, matches, err := remoteRefMatches(command.Git(g), "origin", ref, sha)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("present = false, want true")
	}
	if !matches {
		t.Fatal("matches = false, want true")
	}

	// present but pointing at a different sha.
	other := "2222222222222222222222222222222222222222"
	g2 := fakeRunner(firstTwo, map[string]string{"ls-remote origin": other + "\trefs/heads/" + ref + "\n"}, nil)
	present, matches, err = remoteRefMatches(command.Git(g2), "origin", ref, sha)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("present = false, want true")
	}
	if matches {
		t.Fatal("matches = true, want false")
	}

	// absent (empty ls-remote output).
	g3 := &command.Recorder{}
	present, matches, err = remoteRefMatches(command.Git(g3), "origin", ref, sha)
	if err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatal("present = true, want false")
	}
	if matches {
		t.Fatal("matches = true, want false")
	}
}

func TestPushRefs(t *testing.T) {
	branch, tag := testPrefix+"-v3.33-2", "v3.33.0-2.0-"+testSuffix
	for _, tc := range []struct {
		name           string
		local          bool
		forceWithLease bool
		want           []string
	}{
		{name: "pushes the refs", want: []string{"push", "origin", branch, tag}},
		{name: "local", local: true, want: []string{"push", "origin", "--dry-run", branch, tag}},
		{name: "force with lease", forceWithLease: true, want: []string{"push", "origin", "--force-with-lease", branch, tag}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &command.Recorder{}
			if err := pushRefs(command.Git(g), "origin", tc.local, tc.forceWithLease, branch, tag); err != nil {
				t.Fatalf("pushRefs() unexpected error: %v", err)
			}
			if diff := cmp.Diff([][]string{tc.want}, gitArgs(g)); diff != "" {
				t.Errorf("git calls (-want +got):\n%s", diff)
			}
		})
	}
}

// fakeGit is a git keyed by the full command.
func fakeGit(out map[string]string, errs map[string]error) command.GitRunner {
	return command.Git(fakeRunner(allArgs, out, errs))
}

func TestCheckBranchCurrent(t *testing.T) {
	src := testPrefix + "-v3.33-1"
	up := "origin/" + src
	const sha = "1111111111111111111111111111111111111111"
	const other = "2222222222222222222222222222222222222222"

	// equalTips scripts local and remote tips for the explicit-remote path.
	equalTips := func(local, remote string) map[string]string {
		return map[string]string{
			"rev-parse --verify --quiet " + src:                             local,
			"rev-parse --verify --quiet " + up:                              remote,
			"rev-parse --abbrev-ref --symbolic-full-name master@{upstream}": "upstream/master",
		}
	}

	const fix = "sync it before cutting, or pass --no-branch-check to cut from the local branch as is"

	t.Run("current: local equals remote", func(t *testing.T) {
		g := fakeGit(equalTips(sha, sha), nil)
		if err := checkBranchCurrent(g, src, "origin", true, fix); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("diverged + branch-check: error carries the recovery hint", func(t *testing.T) {
		g := fakeGit(equalTips(sha, other), nil)
		err := checkBranchCurrent(g, src, "origin", true, fix)
		if err == nil || !strings.Contains(err.Error(), "not in sync") {
			t.Fatalf("err = %v, want it to contain %q", err, "not in sync")
		}
		if err == nil || !strings.Contains(err.Error(), "--no-branch-check") {
			t.Fatalf("err = %v, want it to contain %q", err, "--no-branch-check")
		}
	})

	t.Run("diverged + no-branch-check: warn, no error", func(t *testing.T) {
		g := fakeGit(equalTips(sha, other), nil)
		if err := checkBranchCurrent(g, src, "origin", false, fix); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("main failure uses its own recovery hint, not --no-branch-check", func(t *testing.T) {
		g := fakeGit(equalTips(sha, other), nil)
		err := checkBranchCurrent(g, src, "origin", true, "run `git checkout main && git pull` before cutting")
		if err == nil || !strings.Contains(err.Error(), "git checkout main && git pull") {
			t.Fatalf("err = %v, want it to contain %q", err, "git checkout main && git pull")
		}
		if strings.Contains(err.Error(), "--no-branch-check") {
			t.Fatalf("%q contains %q", err.Error(), "--no-branch-check")
		}
	})

	t.Run("absent on remote + branch-check: error", func(t *testing.T) {
		// remote tip resolves empty -> not current.
		g := fakeGit(map[string]string{"rev-parse --verify --quiet " + src: sha}, nil)
		err := checkBranchCurrent(g, src, "origin", true, fix)
		if err == nil || !strings.Contains(err.Error(), "not in sync") {
			t.Fatalf("err = %v, want it to contain %q", err, "not in sync")
		}
	})

	t.Run("empty remote, no configured upstream: error, no fetch", func(t *testing.T) {
		g := fakeRunner(allArgs, nil, map[string]error{
			"rev-parse --abbrev-ref --symbolic-full-name master@{upstream}": errFake,
		})
		err := checkBranchCurrent(command.Git(g), "master", "", true, fix)
		if err == nil || !strings.Contains(err.Error(), "no valid upstream") {
			t.Fatalf("err = %v, want it to contain %q", err, "no valid upstream")
		}
		if ran(g, "fetch") {
			t.Fatal("a missing upstream must fail before fetching")
		}
	})

	t.Run("empty remote reads configured upstream", func(t *testing.T) {
		g := fakeGit(map[string]string{
			"rev-parse --abbrev-ref --symbolic-full-name master@{upstream}": "upstream/master",
			"rev-parse --verify --quiet master":                             sha,
			"rev-parse --verify --quiet upstream/master":                    sha,
		}, nil)
		if err := checkBranchCurrent(g, "master", "", true, fix); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCreateBranch(t *testing.T) {
	g := &command.Recorder{}
	if err := createBranch(command.Git(g), testPrefix+"-v3.33-2", testPrefix+"-v3.33-1"); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([][]string{{"branch", testPrefix + "-v3.33-2", testPrefix + "-v3.33-1"}}, gitArgs(g)); diff != "" {
		t.Fatalf("git calls (-want +got):\n%s", diff)
	}
}

func TestTagWithEmptyCommit(t *testing.T) {
	branch := testPrefix + "-v3.33-2"
	tag := "v3.33.0-2.0-" + testSuffix

	t.Run("commits and tags on the branch", func(t *testing.T) {
		g := &command.Recorder{}
		if err := tagWithEmptyCommit(command.Git(g), branch, tag); err != nil {
			t.Fatal(err)
		}
		want := [][]string{
			{"checkout", branch},
			{"commit", "--allow-empty", "-m", "Begin development for " + tag},
			{"tag", "-f", "--no-sign", tag},
		}
		if diff := cmp.Diff(want, gitArgs(g)); diff != "" {
			t.Fatalf("git calls (-want +got):\n%s", diff)
		}
	})

	t.Run("a failed checkout names the branch and stops", func(t *testing.T) {
		g := fakeRunner(firstTwo, nil, map[string]error{"checkout " + branch: errFake})
		err := tagWithEmptyCommit(command.Git(g), branch, tag)
		if err == nil || !strings.Contains(err.Error(), branch) {
			t.Fatalf("err = %v, want it to contain %q", err, branch)
		}
		if got := len(g.Calls()); got != 1 {
			t.Fatalf("ran %d git calls, want 1", got)
		}
	})
}
