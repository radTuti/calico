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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/release/internal/branch"
	"github.com/projectcalico/calico/release/internal/charts"
	"github.com/projectcalico/calico/release/internal/command"
	"github.com/projectcalico/calico/release/internal/github"
	"github.com/projectcalico/calico/release/internal/manifests"
	"github.com/projectcalico/calico/release/internal/outputs"
	"github.com/projectcalico/calico/release/internal/plan"
	"github.com/projectcalico/calico/release/internal/version"
)

type beforePrepDriver struct {
	Do, Done func() error
}

// branchChangedPaths are the trees the prepareDerived hook rewrites; it reports
// them so the branch flow stages them into the cut commit.
var branchChangedPaths = []string{
	charts.DirName,
	manifests.DirName,
	".semaphore",
	"test-tools",
}

// CutBranch cuts the release branch off main and advances main to the next
// dev line.
func (r *CalicoManager) CutBranch() error {
	if err := r.repo.Validate(); err != nil {
		return err
	}
	c, err := r.cut()
	if err != nil {
		return err
	}
	nodes, err := r.branchDriver().Cut(c)
	if err != nil {
		return err
	}
	return r.runPlan(nodes)
}

func (r *CalicoManager) branchDriver() *branch.Driver {
	return branch.New(
		branch.WithRunner(r.runner),
		branch.WithDir(r.repoRoot),
		branch.WithRemote(r.repo.Remote),
		branch.WithPublish(r.gitRef),
		branch.WithPlanOptions(r.planOptions),
	)
}

// cut is a release-vX.Y branch off main. Only main gets a new tag (next
// minor); the branch inherits main's tag via the shared commit.
func (r *CalicoManager) cut() (branch.Cut, error) {
	var errs []error
	if r.devTagIdentifier == "" {
		errs = append(errs, errors.New("no dev tag identifier specified"))
	}
	if r.releaseBranchPrefix == "" {
		errs = append(errs, errors.New("no release branch prefix specified"))
	}
	if err := errors.Join(errs...); err != nil {
		return branch.Cut{}, err
	}
	// Read the version from main, not HEAD: the branch is cut from main and its
	// name and tags must derive from main's commit.
	g := command.Git(r.runner).In(r.repoRoot)
	mainVersion, err := g.LatestTag(r.mainBranch)
	if err != nil {
		return branch.Cut{}, fmt.Errorf("determining %s version: %w", r.mainBranch, err)
	}
	// After tag-main, main's tip carries the next dev tag; a resume cuts the
	// version before it.
	subject, err := g.Run("log", "-1", "--format=%s", r.mainBranch)
	if err != nil {
		return branch.Cut{}, fmt.Errorf("reading %s tip: %w", r.mainBranch, err)
	}
	if strings.TrimSpace(subject) == branch.DevCommitMessage(mainVersion) {
		if mainVersion, err = g.LatestTag(r.mainBranch + "^"); err != nil {
			return branch.Cut{}, fmt.Errorf("determining %s version before its dev tag: %w", r.mainBranch, err)
		}
	}
	cur, err := version.New(mainVersion)
	if err != nil {
		return branch.Cut{}, err
	}
	nextBranch := cur.NextBranchVersion()
	return branch.Cut{
		Main:    r.mainBranch,
		Source:  r.mainBranch,
		Derived: version.NewReleaseBranch(r.releaseBranchPrefix, cur).String(),
		TagTargets: []branch.TagTarget{
			{Branch: r.mainBranch, DevTag: nextBranch.String() + "-" + r.devTagIdentifier},
		},
		Validate:       r.validate,
		BranchCheck:    r.validateBranch,
		PrepareDerived: r.prepareDerived,
	}, nil
}

// derivedBranchEdits returns the edits to make in the freshly-cut-branch.
func derivedBranchEdits(derived, stream string) []branch.Edit {
	return []branch.Edit{
		{File: "test-tools/mocknode/mock-node.yaml", Pattern: `([a-zA-Z .]+)([a-zA-Z.]+/mock-node:)[^[:space:]]+`, Replacement: fmt.Sprintf(`${1}${2}%s`, derived)},
		{File: "process/testing/aso/export-env.sh", Pattern: `export RELEASE_STREAM="\$\{RELEASE_STREAM:=[a-zA-Z0-9.-]+\}"`, Replacement: fmt.Sprintf(`export RELEASE_STREAM="$${RELEASE_STREAM:=%s}"`, stream)},
		{File: "process/testing/aso/install-calico.sh", Pattern: `: \$\{RELEASE_STREAM:="[a-zA-Z0-9.-]+"\} # Default to [a-zA-Z0-9.-]+`, Replacement: fmt.Sprintf(`: $${RELEASE_STREAM:="%s"} # Default to %s`, stream, stream)},
	}
}

// prepareDerived is the PrepareDerived hook: on a fresh branch it applies the
// edits, rewrites helm values, and runs code generation, returning changed files.
func (r *CalicoManager) prepareDerived(derived string) ([]string, error) {
	rb, ok := version.ParseReleaseBranch(r.releaseBranchPrefix, derived)
	if !ok {
		return nil, fmt.Errorf("%s is not a %s release branch", derived, r.releaseBranchPrefix)
	}
	written, _, err := branch.ApplyEdits(r.repoRoot, derivedBranchEdits(derived, rb.Stream))
	if err != nil {
		return nil, err
	}

	// The operator ships on Calico's version stream, so a pre-release branch
	// pins both to the derived branch.
	logrus.WithField("version", derived).Debug("Updating versions in helm charts to release branches")
	if err := r.modifyHelmChartsValues(derived, derived); err != nil {
		return nil, err
	}

	logrus.Debug("Running code generation")
	env := []string{fmt.Sprintf("DEFAULT_BRANCH_OVERRIDE=%s", derived)}
	if _, err := command.Make(r.runner, r.repoRoot).Run([]string{"generate"}, command.WithEnv(env...)); err != nil {
		return nil, fmt.Errorf("failed to run code generation: %w", err)
	}

	// The helm and generate steps rewrite these tracked trees; stage them alongside
	// the edit-written files.
	changed := append(written, branchChangedPaths...)
	return changed, nil
}

// PrepareRelease opens a PR that carries the release's version edits into the
// current release branch.
func (r *CalicoManager) PrepareRelease() error {
	base, err := r.determineBranch()
	if err != nil {
		return err
	}
	ver := r.calicoVersion.String()
	msg := fmt.Sprintf("build: %s release", ver)
	trailer := gitTrailerReleaseBranch + ": " + base
	core, err := r.branchDriver().Prep(branch.Prep{
		Base:          base,
		Branch:        r.prepBranchName(),
		CommitMessage: msg,
		Trailers:      []string{trailer},
		Edit: func() ([]string, error) {
			if err := r.modifyHelmChartsValues(ver, r.operatorVersion); err != nil {
				return nil, fmt.Errorf("failed to update chart versions: %w", err)
			}
			if _, err := command.Make(r.runner, r.repoRoot).Run([]string{"generate"}); err != nil {
				return nil, fmt.Errorf("failed to run make generate: %w", err)
			}
			// git add fails on a missing path, and the notes dir is absent until
			// the first notes are written.
			var paths []string
			for _, p := range prepStagedPaths(r.repoRoot) {
				if _, err := os.Stat(p); err == nil {
					paths = append(paths, p)
				} else if !errors.Is(err, fs.ErrNotExist) {
					return nil, err
				}
			}
			return paths, nil
		},
	})
	if err != nil {
		return err
	}
	pr := github.PullRequest{
		Repo:   r.repo,
		Base:   base,
		Head:   r.prepBranchName(),
		Title:  msg,
		Body:   trailer,
		Local:  r.planOptions.Local,
		Runner: r.runner,
	}
	if r.repo.Is(github.DefaultRepo()) {
		pr.Reviewers = []string{r.repo.Owner + "/release-team"}
		pr.Labels = []string{"release-note-not-required", "docs-not-required"}
	}
	before := []plan.Node{
		{Name: prepReleaseStep, Validate: r.prepPrereqs, Do: r.beforePrep.Do, Done: r.beforePrep.Done},
		{
			Name: outputs.ReleaseNotesStep,
			When: func() bool { return r.releaseNotes && r.repo.Is(calicoRepo) },
			Do:   func() error { return r.buildReleaseNotes("", r.calicoVersion) },
			Done: func() error { return r.assertReleaseNotesPresent(r.calicoVersion.String()) },
		},
	}
	after := []plan.Node{{
		Name:     github.PRStep,
		When:     func() bool { return r.gitRef },
		Requires: []string{branch.PushRefsStep},
		Validate: func() error {
			if !r.planOptions.Local && !github.Authenticated(r.runner) {
				return fmt.Errorf("no GitHub token to open the PR: set %s or log in with gh", strings.Join(github.TokenEnvVars, " or "))
			}
			return nil
		},
		Do:   pr.Open,
		Done: pr.Opened,
	}}
	return r.runPlan(slices.Concat(before, core, after))
}

func (r *CalicoManager) prepBranchName() string {
	return "build-" + r.calicoVersion.String()
}

var prepStagedPaths = func(repoRoot string) []string {
	return []string{
		filepath.Join(repoRoot, charts.DirName),
		manifests.Dir(repoRoot),
		filepath.Join(repoRoot, outputs.ReleaseNotesDir),
	}
}

func (r *CalicoManager) prepPrereqs() error {
	if err := r.repo.Validate(); err != nil {
		return err
	}
	if !r.validate {
		logrus.Warn("Skipping release preparation validation")
		return nil
	}

	// Check that the git repo is clean.
	if dirty, err := command.Git(r.runner).In(r.repoRoot).Dirty(); err != nil {
		return fmt.Errorf("failed to check if git repo is clean: %w", err)
	} else if dirty {
		return fmt.Errorf("there are uncommitted changes in the repository, please commit or stash them before preparing the release")
	}

	if r.validateBranch {
		if branch, err := r.determineBranch(); err != nil {
			return fmt.Errorf("failed to determine current git branch: %w", err)
		} else if branch == defaultBranch {
			return fmt.Errorf("cannot cut release from branch: %s", branch)
		}
	}
	if r.gitRef {
		if err := r.checkBranch("^"+regexp.QuoteMeta(r.releaseBranchPrefix), "release branch"); err != nil {
			return err
		}
	}
	if r.validateBranch && r.gitRef {
		// Releases are always cut from the main repo, never a fork.
		out, err := r.git("config", "--get", fmt.Sprintf("remote.%s.url", r.repo.Remote))
		if err != nil {
			return fmt.Errorf("failed to get remote %s url: %w", r.repo.Remote, err)
		}
		owner, err := github.OwnerFromRemoteURL(strings.TrimSpace(out))
		if err != nil {
			return err
		}
		if owner != r.repo.Owner {
			return fmt.Errorf("current git remote is %s, expected to be from %s. Please switch to a branch from the main repo to cut the release", owner, r.repo.Owner)
		}
	}

	// Check that the versions are release version.
	if !operatorReleaseVersionRe.MatchString(r.operatorVersion) {
		return fmt.Errorf("operator version (%s) is not a release version", r.operatorVersion)
	}
	if !releaseVersionRe.MatchString(r.calicoVersion.String()) {
		return fmt.Errorf("version (%s) is not a release version", r.calicoVersion.String())
	}

	return nil
}
