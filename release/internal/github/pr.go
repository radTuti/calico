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

package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v53/github"
	"github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/release/internal/command"
)

const PRStep = "pr"

type PullRequestService interface {
	List(ctx context.Context, owner, repo string, opts *github.PullRequestListOptions) ([]*github.PullRequest, *github.Response, error)
	Create(ctx context.Context, owner, repo string, pull *github.NewPullRequest) (*github.PullRequest, *github.Response, error)
	RequestReviewers(ctx context.Context, owner, repo string, number int, reviewers github.ReviewersRequest) (*github.PullRequest, *github.Response, error)
}

type LabelService interface {
	AddLabelsToIssue(ctx context.Context, owner, repo string, number int, labels []string) ([]*github.Label, *github.Response, error)
}

// PullRequest opens a PR from Head, a branch in Repo, into Base.
type PullRequest struct {
	Repo      Repo
	Base      string
	Head      string
	Title     string
	Body      string
	Reviewers []string
	Labels    []string

	// Local looks up an existing PR but opens nothing: the API has no dry run.
	Local bool

	// Runner resolves the token when it falls back to gh.
	Runner command.CommandRunner

	pulls  PullRequestService
	issues LabelService
}

// Open reuses an open PR for Head, so a re-run passes and finishes the
// reviewers and labels a failed run left off.
func (p PullRequest) Open() error {
	ctx := context.Background()
	p = p.withClient()
	log := logrus.WithFields(logrus.Fields{"repo": p.Repo.FullName(), "base": p.Base, "head": p.Head})
	// An anonymous lookup fails on a private repo.
	if p.Local && !Authenticated(p.Runner) {
		log.WithFields(logrus.Fields{"title": p.Title, "reviewers": p.Reviewers, "labels": p.Labels}).Info("Would open PR, without checking for an open one: no GitHub token")
		return nil
	}
	pr, err := p.find(ctx)
	if err != nil {
		return err
	}
	switch {
	case pr != nil:
		log.WithField("PR", pr.GetHTMLURL()).Warn("PR already exists")
	case p.Local:
		log.WithFields(logrus.Fields{"title": p.Title, "reviewers": p.Reviewers, "labels": p.Labels}).Info("Would open PR")
		return nil
	default:
		if pr, _, err = p.pulls.Create(ctx, p.Repo.Owner, p.Repo.Name, &github.NewPullRequest{
			Title: github.String(p.Title),
			Head:  github.String(p.Head),
			Base:  github.String(p.Base),
			Body:  github.String(p.Body),
		}); err != nil {
			return fmt.Errorf("open PR: %w", err)
		}
		log.WithField("PR", pr.GetHTMLURL()).Info("Opened PR")
	}
	if p.Local {
		return nil
	}
	return p.finish(ctx, pr)
}

// Opened ignores labels and reviewers: people change those once the PR is open.
func (p PullRequest) Opened() error {
	pr, err := p.withClient().find(context.Background())
	if err != nil {
		return err
	}
	if pr == nil {
		return fmt.Errorf("no open PR from %s to %s", p.Head, p.Base)
	}
	return nil
}

func (p PullRequest) withClient() PullRequest {
	if p.pulls == nil || p.issues == nil {
		c := Client(p.Runner)
		p.pulls, p.issues = c.PullRequests, c.Issues
	}
	return p
}

// find returns nil with no error when no PR is open.
func (p PullRequest) find(ctx context.Context) (*github.PullRequest, error) {
	open, _, err := p.pulls.List(ctx, p.Repo.Owner, p.Repo.Name, &github.PullRequestListOptions{
		State: "open",
		Head:  p.Repo.Owner + ":" + p.Head,
		Base:  p.Base,
	})
	if err != nil {
		return nil, fmt.Errorf("list open PRs: %w", err)
	}
	if len(open) == 0 {
		return nil, nil
	}
	return open[0], nil
}

func (p PullRequest) finish(ctx context.Context, pr *github.PullRequest) error {
	// Label before anything else so a bot that labels unlabelled PRs finds the
	// labels already set.
	if len(p.Labels) > 0 {
		if _, _, err := p.issues.AddLabelsToIssue(ctx, p.Repo.Owner, p.Repo.Name, pr.GetNumber(), p.Labels); err != nil {
			return fmt.Errorf("label %s: %w", pr.GetHTMLURL(), err)
		}
	}
	if len(p.Reviewers) > 0 {
		var req github.ReviewersRequest
		for _, r := range p.Reviewers {
			if _, team, ok := strings.Cut(r, "/"); ok {
				req.TeamReviewers = append(req.TeamReviewers, team)
			} else {
				req.Reviewers = append(req.Reviewers, r)
			}
		}
		if _, _, err := p.pulls.RequestReviewers(ctx, p.Repo.Owner, p.Repo.Name, pr.GetNumber(), req); err != nil {
			return fmt.Errorf("request reviewers on %s: %w", pr.GetHTMLURL(), err)
		}
	}
	return nil
}
