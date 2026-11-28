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

var validPrep = Prep{
	Base:          "release-v3.33",
	Branch:        "build-v3.33.1",
	CommitMessage: "build: v3.33.1 release",
	Edit:          func() ([]string, error) { return nil, nil },
}

// --skip accepts only PrepSteps, so a node missing from it cannot be skipped.
func TestPrepSteps(t *testing.T) {
	nodes, err := New(WithRunner(noRefs()), WithPublish(true)).Prep(validPrep)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(PrepSteps(), skippableNames(nodes)); diff != "" {
		t.Fatalf("PrepSteps() vs the prep's nodes (-want +got):\n%s", diff)
	}
}

func TestPrep(t *testing.T) {
	valid := validPrep
	for _, tc := range []struct {
		name string
		edit func(*Prep)
		want string
	}{
		{name: "no base", edit: func(p *Prep) { p.Base = "" }, want: "no base branch"},
		{name: "no branch", edit: func(p *Prep) { p.Branch = "" }, want: "no prep branch"},
		{name: "no commit message", edit: func(p *Prep) { p.CommitMessage = "" }, want: "no commit message"},
		{name: "no edit", edit: func(p *Prep) { p.Edit = nil }, want: "no edit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			tc.edit(&p)
			_, err := New().Prep(p)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}

	// A skipped push-refs' Done passes against the last run's push.
	t.Run("a skipped push-refs is stale while the prep branch is rebuilt", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			skip []string
		}{
			{name: "push-refs alone", skip: []string{PushRefsStep}},
			{name: "with update-derived", skip: []string{stepUpdateDerived, PushRefsStep}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r := &command.Recorder{}
				opts := plan.Options{Skip: map[string]bool{}}
				for _, s := range tc.skip {
					opts.Skip[s] = true
				}
				nodes, err := New(WithRunner(r), WithRemote("origin"), WithPublish(true), WithPlanOptions(opts)).Prep(valid)
				if err != nil {
					t.Fatal(err)
				}
				pr := plan.Node{Name: "pr", Requires: []string{PushRefsStep}, Do: func() error { return nil }}
				p, err := plan.New(opts, append(nodes, pr)...)
				if err != nil {
					t.Fatal(err)
				}
				if err := p.Run(); err == nil || !strings.Contains(err.Error(), "would be stale") {
					t.Fatalf("err = %v, want it to contain %q", err, "would be stale")
				}
				if len(r.Calls()) > 0 {
					t.Fatalf("git calls = %v, want none", r.Calls())
				}
			})
		}
	})
}
