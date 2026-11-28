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
	"github.com/projectcalico/calico/release/internal/steps"
)

// Step names shared by every branch plan. --skip validates its values against
// the list for the command.
const (
	stepCutBranch     = "cut-branch"
	stepUpdateDerived = "update-derived"
	stepTagDerived    = "tag-derived"
	stepUpdateMain    = "update-main"
	stepTagMain       = "tag-main"
	PushRefsStep      = "push-refs"
	stepSwitchBack    = "switch-back"
)

// Driver builds the git nodes of a branch plan in one repository. They share
// HEAD, so their order is fixed here; a caller adds its own nodes around them.
type Driver struct {
	steps.Step
	opts    plan.Options
	remote  string
	publish bool // whether to write to the remote
}

func (d *Driver) nodes(g command.GitRunner, j spec) []plan.Node {
	// update-main and tag-main both act on the main target's branch when main
	// advances; the derived branch is tagged separately.
	mainTgt, hasMainTgt := j.mainTarget()
	derivedTgt, hasDerivedTgt := j.derivedTarget()
	// A run ends where a fresh run starts: on a branch source, else on main.
	home := j.source
	if strings.HasPrefix(home, tagRefPrefix) {
		home = j.main
	}
	switchBack := func() error {
		args := []string{"switch", home}
		if j.scratch {
			args = []string{"switch", "-f", home}
		}
		if _, err := g.Run(args...); err != nil {
			return fmt.Errorf("switch back to %s: %w", home, err)
		}
		return nil
	}
	cut := plan.Node{
		Name:     stepCutBranch,
		Do:       func() error { return cutBranch(g, j) },
		Done:     func() error { return refDone(localBranchExists(g, j.derived)) },
		Rollback: switchBack,
	}
	update := plan.Node{
		Name:     stepUpdateDerived,
		When:     func() bool { return j.edit != nil },
		Requires: []string{stepCutBranch},
		Do:       func() error { return updateDerived(g, j) },
		Done:     func() error { return hasCommit(g, j.source, j.derived, j.commitMessage) },
	}
	// Require only nodes this spec turns on: one off by When has no Done.
	derivedDone := stepUpdateDerived
	if j.edit == nil {
		derivedDone = stepCutBranch
	}
	pushRequires := []string{derivedDone}
	if hasDerivedTgt {
		pushRequires = append(pushRequires, stepTagDerived)
	}
	if hasMainTgt {
		pushRequires = append(pushRequires, stepTagMain)
	}
	return []plan.Node{
		{
			// Not in any skip list, so --skip cannot name it.
			Name:     stepPrepCut,
			When:     func() bool { return j.check != nil },
			Validate: func() error { return j.check() },
		},
		cut,
		update,
		{
			Name: stepTagDerived,
			// Runs while HEAD is still on the derived branch, before update-main
			// switches away. A cut off main has no derived tag: the branch
			// inherits main's tag.
			When:     func() bool { return hasDerivedTgt },
			Requires: []string{derivedDone},
			Do:       func() error { return tagHead(g, derivedTgt.Branch, derivedTgt.DevTag) },
			Done:     func() error { return refDone(localTagPointsAt(g, derivedTgt.DevTag, derivedTgt.Branch)) },
		},
		{
			Name:     stepUpdateMain,
			When:     func() bool { return hasMainTgt && j.prepareMain != nil },
			Requires: []string{stepCutBranch},
			Do:       func() error { return updateMain(g, j.prepareMain, mainTgt.Branch) },
			Done:     func() error { return hasCommit(g, j.derived, mainTgt.Branch, mainCommitMessage(mainTgt.Branch)) },
		},
		{
			// Not update-main: a cut may skip it and tag main as it stands.
			Name:     stepTagMain,
			When:     func() bool { return hasMainTgt },
			Requires: []string{stepCutBranch},
			Do: func() error {
				// A re-run must not stack a second "Begin development" commit.
				tagged, err := localTagPointsAt(g, mainTgt.DevTag, mainTgt.Branch)
				if err != nil || tagged {
					return err
				}
				return tagWithEmptyCommit(g, mainTgt.Branch, mainTgt.DevTag)
			},
			Done: func() error { return refDone(localTagPointsAt(g, mainTgt.DevTag, mainTgt.Branch)) },
		},
		{
			Name:     PushRefsStep,
			When:     func() bool { return d.publish },
			Requires: pushRequires,
			Validate: d.requireRemote,
			Do:       func() error { return pushRefs(g, d.remote, d.opts.Local, j.scratch, j.pushRefs()...) },
			Done:     func() error { return d.refsPushed(g, j) },
		},
		{
			// Not in any skip list, so --skip cannot name it.
			Name: stepSwitchBack,
			Do:   switchBack,
			Done: func() error {
				cur, err := g.Branch()
				if err != nil {
					return err
				}
				if cur = strings.TrimSpace(cur); cur != home {
					return fmt.Errorf("on %s, not %s", cur, home)
				}
				return nil
			},
		},
	}
}

// refsPushed passes only when every ref is on the remote at the local commit.
func (d *Driver) refsPushed(g command.GitRunner, j spec) error {
	for _, r := range j.pushRefs() {
		if err := remoteRefAtLocal(g, d.remote, r); err != nil {
			return err
		}
	}
	return nil
}

func (d *Driver) requireRemote() error {
	if d.remote == "" {
		return errors.New("no git remote to push to")
	}
	return nil
}

// pushRefs includes the refs of skipped steps: push-refs requires those steps,
// so their refs exist.
func (j spec) pushRefs() []string {
	refs := []string{j.derived}
	if t, ok := j.derivedTarget(); ok {
		refs = append(refs, t.DevTag)
	}
	if mt, ok := j.mainTarget(); ok {
		refs = append(refs, mt.Branch, mt.DevTag)
	}
	return dedupe(refs)
}

func New(opts ...Option) *Driver {
	d := &Driver{}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

type Option func(*Driver)

func WithRunner(r command.CommandRunner) Option {
	return func(d *Driver) { d.Apply([]steps.Option{steps.WithRunner(r)}) }
}

// WithDir sets the repository root.
func WithDir(dir string) Option {
	return func(d *Driver) { d.Apply([]steps.Option{steps.WithDir(dir)}) }
}

func WithRemote(remote string) Option {
	return func(d *Driver) { d.remote = remote }
}

func WithPublish(publish bool) Option {
	return func(d *Driver) { d.publish = publish }
}

// WithPlanOptions passes the run's options: Local and Skip change what the
// nodes do, so they must match the options the plan runs with.
func WithPlanOptions(o plan.Options) Option {
	return func(d *Driver) { d.opts = o }
}

// spec is one branch plan: derived starts from source and carries the edit
// commit. A cut also tags and advances main.
type spec struct {
	main    string
	source  string
	derived string

	// scratch resets derived to source on every run, so its push needs
	// --force-with-lease and a failed run may discard what it left behind.
	scratch bool

	check         func() error
	edit          func(derived string) ([]string, error)
	commitMessage string
	trailers      []string

	tagTargets  []TagTarget
	prepareMain func(main string) ([]string, error)
}

// refDone turns a ref probe into a plan Done result.
func refDone(present bool, err error) error {
	if err != nil {
		return err
	}
	if !present {
		return errors.New("missing")
	}
	return nil
}

func cutBranch(g command.GitRunner, j spec) error {
	if j.scratch {
		if _, err := g.Run("switch", "-C", j.derived, j.source); err != nil {
			return fmt.Errorf("create branch %s: %w", j.derived, err)
		}
		return nil
	}
	exists, err := localBranchExists(g, j.derived)
	if err != nil || exists {
		return err
	}
	return createAndCheckoutBranch(g, j.derived, j.source)
}

// updateDerived runs the edit on the derived branch and commits the files it
// changed.
func updateDerived(g command.GitRunner, j spec) error {
	// If source advanced past the derived branch, re-cut it from source so the
	// resume picks up the new source commits; otherwise just check it out.
	ahead, err := sourceAheadOf(g, j.source, j.derived)
	if err != nil {
		return err
	}
	if ahead {
		logrus.WithFields(logrus.Fields{"branch": j.derived, "source": j.source, "step": stepUpdateDerived}).
			Info("source advanced; resetting derived branch to source")
		if err := resetBranchToBase(g, j.derived, j.source); err != nil {
			return err
		}
	} else if err := checkoutBranch(g, j.derived); err != nil {
		return err
	}
	changed, err := j.edit(j.derived)
	if err != nil {
		return fmt.Errorf("prepare branch: %w", err)
	}
	committed, err := stageAndCommit(g, j.commitMessage, j.trailers, dedupe(changed))
	if err != nil {
		return fmt.Errorf("stage and commit: %w", err)
	}
	if !committed {
		logrus.WithFields(logrus.Fields{"branch": j.derived, "step": stepUpdateDerived}).Info("no changes to commit")
	}
	return nil
}

// hasCommit fails when an edit changed nothing, since no commit was made.
func hasCommit(g command.GitRunner, from, to, subject string) error {
	out, err := g.Run("log", "--format=%s", from+".."+to)
	if err != nil {
		return err
	}
	if !slices.Contains(strings.Split(out, "\n"), subject) {
		return fmt.Errorf("no %q commit on %s since %s", subject, to, from)
	}
	return nil
}

func dedupe(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
