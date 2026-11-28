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
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/projectcalico/calico/release/internal/command"
	"github.com/projectcalico/calico/release/internal/plan"
)

// A bad cut is rejected before any git work.
func TestCut(t *testing.T) {
	for _, tc := range []struct {
		name string
		cut  Cut
		want string
	}{
		{name: "source equals derived", cut: Cut{Main: "master", Derived: "release-v3.33", Source: "release-v3.33"}, want: "cannot be the same as derived"},
		{name: "no main", cut: Cut{Derived: "release-v3.33", Source: "master"}, want: "no main branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New().Cut(tc.cut)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	// A caller's nodes go after the cut's, so the cut must end on its source.
	t.Run("switch-back runs last", func(t *testing.T) {
		d, g := stepDriver(t.TempDir(), noRefs(), plan.Options{})
		nodes := cutNodes(d, g, Cut{Derived: testPrefix + "-v3.33"})
		if diff := cmp.Diff(stepSwitchBack, nodes[len(nodes)-1].Name); diff != "" {
			t.Fatalf("nodes[len(nodes)-1].Name (-want +got):\n%s", diff)
		}
	})

	// A node off by When has no Done, so requiring it would fail every run.
	t.Run("push-refs requires only the nodes the cut turns on", func(t *testing.T) {
		edit := func(string) ([]string, error) { return nil, nil }
		for _, tc := range []struct {
			name string
			edit func(string) ([]string, error)
			want []string
		}{
			{name: "with a derived edit", edit: edit, want: []string{stepUpdateDerived, stepTagMain}},
			{name: "without a derived edit", want: []string{stepCutBranch, stepTagMain}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				d, g := stepDriver(t.TempDir(), noRefs(), plan.Options{})
				c := Cut{Derived: testPrefix + "-v3.33", PrepareDerived: tc.edit, TagTargets: []TagTarget{{Branch: "master", DevTag: "v3.34.0-" + testSuffix}}}
				push := findNode(t, cutNodes(d, g, c), PushRefsStep)
				if diff := cmp.Diff(tc.want, push.Requires); diff != "" {
					t.Fatalf("push.Requires (-want +got):\n%s", diff)
				}
			})
		}
	})
}

func TestTagMainNode(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"
	c := Cut{Derived: testPrefix + "-v3.34", TagTargets: []TagTarget{{Branch: "master", DevTag: "v3.35.0-" + testSuffix}}}

	t.Run("a re-run adds no commit", func(t *testing.T) {
		r := &command.Recorder{Reply: func(command.Call) (string, error) { return sha + "\n", nil }}
		d, g := stepDriver(t.TempDir(), r, plan.Options{})
		tagMain := findNode(t, cutNodes(d, g, c), stepTagMain)
		if err := tagMain.Do(); err != nil {
			t.Fatal(err)
		}
		if ran(r, "commit") {
			t.Fatal("ran git commit, want it left alone")
		}
	})

	t.Run("off without a main target", func(t *testing.T) {
		d, g := stepDriver(t.TempDir(), noRefs(), plan.Options{})
		if findNode(t, cutNodes(d, g, Cut{Derived: c.Derived}), stepTagMain).When() {
			t.Fatal("findNode(t, cutNodes(d, g, Cut{Derived: c.Derived}), stepTagMain).When() = true, want false")
		}
	})
}
