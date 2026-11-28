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
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/projectcalico/calico/release/internal/command"
	"github.com/projectcalico/calico/release/internal/plan"
)

// noRefs fails every rev-parse, as in a repo with no matching ref.
func noRefs() *command.Recorder {
	return &command.Recorder{Reply: func(c command.Call) (string, error) {
		if c.Args[0] == "rev-parse" {
			return "", errFake
		}
		return "", nil
	}}
}

func findNode(t *testing.T, nodes []plan.Node, name string) plan.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("node not found: %s", name)
	return plan.Node{}
}

// stepDriver returns a Driver at repoRoot and the git its nodes use. A nil r
// runs real git.
func stepDriver(repoRoot string, r command.CommandRunner, opts plan.Options) (*Driver, command.GitRunner) {
	d := New(WithDir(repoRoot), WithRunner(r), WithRemote("origin"), WithPlanOptions(opts))
	return d, d.Git()
}

// cutNodes returns the nodes of c's cut.
func cutNodes(d *Driver, g command.GitRunner, c Cut) []plan.Node {
	return d.nodes(g, d.cutSpec(g, c))
}

// runNodes runs the named nodes of d's cut, minus requirements on the rest.
func runNodes(t *testing.T, d *Driver, g command.GitRunner, c Cut, names ...string) error {
	t.Helper()
	var nodes []plan.Node
	for _, name := range names {
		n := findNode(t, cutNodes(d, g, c), name)
		n.Requires = slices.DeleteFunc(slices.Clone(n.Requires), func(r string) bool { return !slices.Contains(names, r) })
		nodes = append(nodes, n)
	}
	pl, err := plan.New(d.opts, nodes...)
	if err != nil {
		t.Fatal(err)
	}
	return pl.Run()
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newTestDriver(t *testing.T, root string, opts ...Option) *Driver {
	t.Helper()
	return New(append([]Option{WithDir(root), WithRemote("origin")}, opts...)...)
}

// runCut runs c as a plan, the way the manager does.
func runCut(d *Driver, c Cut) error {
	if c.Main == "" {
		c.Main = "master"
	}
	nodes, err := d.Cut(c)
	if err != nil {
		return err
	}
	p, err := plan.New(d.opts, nodes...)
	if err != nil {
		return err
	}
	return p.Run()
}

// A repo with other values sets them in an init().
var (
	testPrefix = "release"
	testSuffix = "0.dev"
)

func TestSwitchBackNode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		head    string
		wantErr bool
	}{
		{name: "done on the source", head: "master\n"},
		{name: "not done on another branch", head: testPrefix + "-v3.33\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &command.Recorder{Reply: func(command.Call) (string, error) { return tc.head, nil }}
			d, g := stepDriver(t.TempDir(), r, plan.Options{})
			err := findNode(t, cutNodes(d, g, Cut{Main: "master", Source: "master", Derived: testPrefix + "-v3.33"}), stepSwitchBack).Done()
			if (err != nil) != tc.wantErr {
				t.Errorf("Done() error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

func TestCutBranchNode(t *testing.T) {
	c := Cut{Derived: testPrefix + "-v3.33-2", Source: testPrefix + "-v3.33-1"}

	t.Run("creates a missing branch", func(t *testing.T) {
		r := &command.Recorder{Reply: func(c command.Call) (string, error) {
			if c.Args[0] == "rev-parse" {
				return "", errNoRef
			}
			return "", nil
		}}
		d, g := stepDriver(t.TempDir(), r, plan.Options{})
		cut := findNode(t, cutNodes(d, g, c), stepCutBranch)
		if err := cut.Done(); err == nil {
			t.Fatal("cut.Done() = nil, want an error")
		}
		if err := cut.Do(); err != nil {
			t.Fatal(err)
		}
		if !ran(r, "branch") {
			t.Fatal("did not run git branch")
		}
	})

	t.Run("keeps an existing branch", func(t *testing.T) {
		r := &command.Recorder{Reply: func(command.Call) (string, error) { return "abc123\n", nil }}
		d, g := stepDriver(t.TempDir(), r, plan.Options{})
		cut := findNode(t, cutNodes(d, g, c), stepCutBranch)
		if err := cut.Done(); err != nil {
			t.Fatal(err)
		}
		if err := cut.Do(); err != nil {
			t.Fatal(err)
		}
		if ran(r, "branch") {
			t.Fatal("ran git branch, want it left alone")
		}
	})
}

// update-derived calls PrepareDerived; plan mode does not.
func TestUpdateDerivedNode(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "metadata.mk", "MANAGER_BRANCH ?= master\n")

	var events []string
	c := Cut{Derived: testPrefix + "-v3.33"}
	prepare := func(derived string) ([]string, error) {
		written, _, err := ApplyEdits(root, []Edit{
			{File: "metadata.mk", Pattern: `^MANAGER_BRANCH.*`, Replacement: "MANAGER_BRANCH ?= release-vX"},
		})
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, "prepare:"+derived)
		return written, nil
	}

	t.Run("preview does not act", func(t *testing.T) {
		d, g := stepDriver(root, noRefs(), plan.Options{Preview: true})
		c.PrepareDerived = prepare
		if err := runNodes(t, d, g, c, stepUpdateDerived); err != nil {
			t.Fatal(err)
		}
		if len(events) > 0 {
			t.Fatalf("events = %v, want empty", events)
		}
		if diff := cmp.Diff("MANAGER_BRANCH ?= master\n", readFile(t, root, "metadata.mk")); diff != "" {
			t.Fatalf("metadata.mk (-want +got):\n%s", diff)
		}
	})

	t.Run("apply runs the hook", func(t *testing.T) {
		r := &command.Recorder{Reply: func(c command.Call) (string, error) {
			switch c.Args[0] {
			case "rev-parse":
				return "", errFake
			case "log":
				return "Updates for " + testPrefix + "-v3.33 release branch\n", nil
			}
			return "", nil
		}}
		d, g := stepDriver(root, r, plan.Options{})
		c.PrepareDerived = prepare
		if err := runNodes(t, d, g, c, stepUpdateDerived); err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff([]string{"prepare:" + testPrefix + "-v3.33"}, events); diff != "" {
			t.Fatalf("events (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff("MANAGER_BRANCH ?= release-vX\n", readFile(t, root, "metadata.mk")); diff != "" {
			t.Fatalf("metadata.mk (-want +got):\n%s", diff)
		}
	})

	t.Run("off without a derived edit", func(t *testing.T) {
		d, g := stepDriver(root, noRefs(), plan.Options{})
		if findNode(t, cutNodes(d, g, Cut{Derived: testPrefix + "-v3.33"}), stepUpdateDerived).When() {
			t.Fatal("update-derived is on without a derived edit")
		}
	})

	t.Run("Done", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			log     string
			wantErr bool
		}{
			{name: "done with the edit commit", log: "Other\nUpdates for " + testPrefix + "-v3.33 release branch\n"},
			{name: "not done without it", log: "Other\n", wantErr: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r := &command.Recorder{Reply: func(command.Call) (string, error) { return tc.log, nil }}
				d, g := stepDriver(t.TempDir(), r, plan.Options{})
				c := Cut{Source: "master", Derived: testPrefix + "-v3.33", PrepareDerived: prepare}
				err := findNode(t, cutNodes(d, g, c), stepUpdateDerived).Done()
				if (err != nil) != tc.wantErr {
					t.Errorf("Done() error = %v, want error %v", err, tc.wantErr)
				}
				if want := []string{"log", "--format=%s", "master.." + testPrefix + "-v3.33"}; !slices.Equal(r.Calls()[0].Args, want) {
					t.Errorf("ran %v, want %v", r.Calls()[0].Args, want)
				}
			})
		}
	})
}

func TestUpdateMainNode(t *testing.T) {
	derived := testPrefix + "-v3.33"
	for _, tc := range []struct {
		name    string
		log     string
		wantErr bool
	}{
		{name: "done with the main commit", log: "Begin development for v3.34.0-" + testSuffix + "\nUpdate master for release branch cut\n"},
		{name: "not done without it", log: "Other\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &command.Recorder{Reply: func(command.Call) (string, error) { return tc.log, nil }}
			d, g := stepDriver(t.TempDir(), r, plan.Options{})
			c := Cut{
				Source:      "master",
				Derived:     derived,
				TagTargets:  []TagTarget{{Branch: "master", DevTag: "v3.34.0-" + testSuffix}},
				PrepareMain: func(string) ([]string, error) { return nil, nil },
			}
			err := findNode(t, cutNodes(d, g, c), stepUpdateMain).Done()
			if (err != nil) != tc.wantErr {
				t.Errorf("Done() error = %v, want error %v", err, tc.wantErr)
			}
			if want := []string{"log", "--format=%s", derived + "..master"}; !slices.Equal(r.Calls()[0].Args, want) {
				t.Errorf("ran %v, want %v", r.Calls()[0].Args, want)
			}
		})
	}
}

func TestPushRefsNode(t *testing.T) {
	const localSha = "1111111111111111111111111111111111111111"
	derived := testPrefix + "-v3.33"
	c := Cut{Derived: derived}

	t.Run("Done", func(t *testing.T) {
		tag := "v3.33.0-2.0-" + testSuffix
		tc2 := Cut{Derived: derived, TagTargets: []TagTarget{{Branch: derived, DevTag: tag}}}
		for _, tc := range []struct {
			name    string
			remote  string
			wantErr bool
		}{
			{name: "stale remote ref", remote: "2222222222222222222222222222222222222222\t\n", wantErr: true},
			{name: "remote ref at the local commit", remote: localSha + "\t\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r := fakeRunner(firstTwo, map[string]string{
					"rev-parse --verify": localSha,
					"ls-remote origin":   tc.remote,
				}, nil)
				d, g := stepDriver(t.TempDir(), r, plan.Options{})
				d.publish = true
				err := findNode(t, cutNodes(d, g, tc2), PushRefsStep).Done()
				if (err != nil) != tc.wantErr {
					t.Errorf("Done() error = %v, want error %v", err, tc.wantErr)
				}
			})
		}
	})

	t.Run("Do", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			local bool
			skip  map[string]bool
			want  [][]string
		}{
			{name: "publishes", want: [][]string{{"push", "origin", derived}}},
			{name: "local checks the push", local: true, want: [][]string{{"push", "origin", "--dry-run", derived}}},
			{name: "pushes the ref of a skipped step", skip: map[string]bool{stepCutBranch: true}, want: [][]string{{"push", "origin", derived}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				g := &command.Recorder{}
				d := New(WithRemote("origin"), WithPublish(true), WithPlanOptions(plan.Options{Local: tc.local, Skip: tc.skip}))
				if err := findNode(t, cutNodes(d, command.Git(g), c), PushRefsStep).Do(); err != nil {
					t.Fatalf("push-refs Do() unexpected error: %v", err)
				}
				if diff := cmp.Diff(tc.want, gitArgs(g)); diff != "" {
					t.Errorf("git calls (-want +got):\n%s", diff)
				}
			})
		}
	})

	t.Run("off when publish is off", func(t *testing.T) {
		if findNode(t, cutNodes(New(), command.Git(&command.Recorder{}), c), PushRefsStep).When() {
			t.Fatal("findNode(t, cutNodes(New(), command.Git(&command.Recorder{}), c), PushRefsStep).When() = true, want false")
		}
	})

	t.Run("Validate needs a remote", func(t *testing.T) {
		d := New(WithPublish(true))
		if err := findNode(t, cutNodes(d, command.Git(&command.Recorder{}), c), PushRefsStep).Validate(); err == nil {
			t.Fatal("findNode(t, cutNodes(d, command.Git(&command.Recorder{}), c), PushRefsStep).Validate() = nil, want an error")
		}
	})
}
