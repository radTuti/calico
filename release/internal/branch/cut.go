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
	"fmt"
	"slices"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/release/internal/command"
	"github.com/projectcalico/calico/release/internal/plan"
)

const stepPrepCut = "prep-cut"

var cutSteps = []string{
	stepCutBranch,
	stepUpdateDerived,
	stepTagDerived,
	stepUpdateMain,
	stepTagMain,
	PushRefsStep,
}

// CutSteps returns the steps a cut can skip, in the order they run.
func CutSteps() []string {
	return slices.Clone(cutSteps)
}

// TagTarget is a branch that gets a dev tag. The derived branch is tagged in
// place; any other target gets a "Begin development" commit first.
type TagTarget struct {
	Branch string
	DevTag string
}

// Cut describes a branch cut: the derived branch, where it comes from, and the
// edits it carries.
type Cut struct {
	Main string

	// Source is a branch, or a tag from TagSource.
	Source  string
	Derived string

	// TagTargets are the branches that get a dev tag: derived and/or main.
	TagTargets []TagTarget

	// Validate turns on the fresh-cut checks for a dirty tree and for a checkout
	// off a branch Source.
	Validate bool

	// BranchCheck requires the source current with its remote; when false a
	// non-current source only warns. Main is always required current.
	BranchCheck bool

	// PrepareDerived does the derived-branch work before its edits commit,
	// returning the changed files to stage.
	PrepareDerived func(derived string) ([]string, error)

	// PrepareMain does the main-branch work before its dev-tag commit,
	// returning the changed files to stage.
	PrepareMain func(main string) ([]string, error)
}

// Cut returns the nodes that cut a release branch, ending back on Source.
// Every node is safe to re-run, so a failed cut resumes by running it again.
func (d *Driver) Cut(c Cut) ([]plan.Node, error) {
	g := d.Git()
	j := d.cutSpec(g, c)
	var errs []error
	if c.Main == "" {
		errs = append(errs, errors.New("no main branch specified"))
	}
	if c.Derived == "" {
		errs = append(errs, errors.New("no derived branch specified"))
	}
	if c.Source == c.Derived {
		errs = append(errs, fmt.Errorf("source %q cannot be the same as derived %q branch", c.Source, c.Derived))
	}
	// The main check is skipped for a tag source, so a tag cut must not advance main.
	if t, ok := j.mainTarget(); ok && strings.HasPrefix(c.Source, tagRefPrefix) {
		errs = append(errs, fmt.Errorf("a cut from %s cannot advance %s", strings.TrimPrefix(c.Source, tagRefPrefix), t.Branch))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return d.nodes(g, j), nil
}

func (d *Driver) cutSpec(g command.GitRunner, c Cut) spec {
	return spec{
		main:          c.Main,
		source:        c.Source,
		derived:       c.Derived,
		check:         func() error { return d.validateFreshCut(g, c) },
		edit:          c.PrepareDerived,
		commitMessage: fmt.Sprintf("Updates for %s release branch", c.Derived),
		tagTargets:    c.TagTargets,
		prepareMain:   c.PrepareMain,
	}
}

// validateFreshCut gates a fresh cut; a resume skips it.
func (d *Driver) validateFreshCut(g command.GitRunner, c Cut) error {
	logrus.WithFields(logrus.Fields{"source": c.Source, "derived": c.Derived, "remote": d.remote}).Info("Cutting release branch")
	for _, t := range c.TagTargets {
		logrus.WithFields(logrus.Fields{"branch": t.Branch, "devTag": t.DevTag}).Info("Dev tag")
	}
	exists, err := localBranchExists(g, c.Derived)
	if err != nil {
		return err
	}
	if exists {
		logrus.WithField("branch", c.Derived).Info("derived branch already exists; skipping fresh-cut checks")
		return nil
	}
	_, fromTag := strings.CutPrefix(c.Source, tagRefPrefix)
	// --no-validation turns off only these checks; the base check always runs.
	if c.Validate {
		if dirty, err := g.Dirty(); err != nil {
			return err
		} else if dirty {
			return fmt.Errorf("there are uncommitted changes in the repository, please commit or stash them before creating a new release branch")
		}
		if !fromTag {
			cur, err := g.Branch()
			if err != nil {
				return fmt.Errorf("determining current branch: %w", err)
			}
			if cur != c.Source {
				return fmt.Errorf("branch cut must run on %s, but the current branch is %s", c.Source, cur)
			}
		}
	} else {
		logrus.Info("skipping the dirty-tree and current-branch checks (validation off)")
	}
	return d.checkBases(g, c)
}

// checkBases verifies each base is current with its remote. Main is always
// required (its upstream is .git/config); the source only under BranchCheck.
func (d *Driver) checkBases(g command.GitRunner, c Cut) error {
	// A cut from a tag never touches main; the tag just has to be the one the
	// remote released.
	if tagName, ok := strings.CutPrefix(c.Source, tagRefPrefix); ok {
		if d.remote == "" {
			return fmt.Errorf("no git remote to check tag %s against", tagName)
		}
		return checkTagOnRemote(g, tagName, d.remote)
	}
	mainFix := fmt.Sprintf("run `git checkout %s && git pull` before cutting", c.Main)
	if err := checkBranchCurrent(g, c.Main, "", true, mainFix); err != nil {
		return fmt.Errorf("check %s is current: %w", c.Main, err)
	}
	if c.Source != c.Main {
		srcFix := "sync it before cutting, or pass --no-branch-check to cut from the local branch as is"
		if err := checkBranchCurrent(g, c.Source, d.remote, c.BranchCheck, srcFix); err != nil {
			return fmt.Errorf("check %s is current: %w", c.Source, err)
		}
	}
	return nil
}

// updateMain runs prepare on the main branch and commits the changed files,
// so a later checkout does not carry a staged main edit.
func updateMain(g command.GitRunner, prepare func(string) ([]string, error), mainBranch string) error {
	if err := checkoutBranch(g, mainBranch); err != nil {
		return err
	}
	changed, err := prepare(mainBranch)
	if err != nil {
		return fmt.Errorf("prepare main (%s): %w", mainBranch, err)
	}
	if _, err := stageAndCommit(g, mainCommitMessage(mainBranch), nil, dedupe(changed)); err != nil {
		return fmt.Errorf("stage and commit: %w", err)
	}
	return nil
}

func mainCommitMessage(mainBranch string) string {
	return fmt.Sprintf("Update %s for release branch cut", mainBranch)
}

// derivedTarget returns the derived branch's tag target, if any. A cut off main
// omits it: the branch inherits main's tag through the shared commit.
func (j spec) derivedTarget() (TagTarget, bool) {
	for _, t := range j.tagTargets {
		if t.Branch == j.derived {
			return t, true
		}
	}
	return TagTarget{}, false
}

// mainTarget returns the non-derived tag target (main's advance), if any.
func (j spec) mainTarget() (TagTarget, bool) {
	for _, t := range j.tagTargets {
		if t.Branch != j.derived {
			return t, true
		}
	}
	return TagTarget{}, false
}
