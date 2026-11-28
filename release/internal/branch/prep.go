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
	"errors"
	"slices"

	"github.com/projectcalico/calico/release/internal/plan"
)

var prepSteps = []string{
	stepCutBranch,
	stepUpdateDerived,
	PushRefsStep,
}

// PrepSteps returns the steps a prep can skip, in the order they run.
func PrepSteps() []string {
	return slices.Clone(prepSteps)
}

// Prep describes a release prep: a branch off Base that carries the release's
// edits.
type Prep struct {
	// Base is the release branch the prep branch starts from and merges into.
	Base string

	// Branch is the prep branch. A re-run resets it to Base.
	Branch string

	CommitMessage string

	// Trailers are "Key: value" lines added to the commit.
	Trailers []string

	// Edit makes the release edits on the prep branch and returns the paths to
	// stage. Changes left in the tree on Base before the plan runs carry over.
	Edit func() ([]string, error)
}

// Prep returns the nodes that commit a release's edits on the prep branch and
// push it, ending back on Base. A failed run also switches back to Base; the
// prep branch keeps whatever was committed.
func (d *Driver) Prep(p Prep) ([]plan.Node, error) {
	var errs []error
	if p.Base == "" {
		errs = append(errs, errors.New("no base branch specified"))
	}
	if p.Branch == "" {
		errs = append(errs, errors.New("no prep branch specified"))
	}
	if p.CommitMessage == "" {
		errs = append(errs, errors.New("no commit message specified"))
	}
	if p.Edit == nil {
		errs = append(errs, errors.New("no edit specified"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return d.nodes(d.Git(), prepSpec(p)), nil
}

func prepSpec(p Prep) spec {
	return spec{
		main:          p.Base,
		source:        p.Base,
		derived:       p.Branch,
		scratch:       true,
		edit:          func(string) ([]string, error) { return p.Edit() },
		commitMessage: p.CommitMessage,
		trailers:      p.Trailers,
	}
}
