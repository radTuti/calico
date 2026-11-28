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
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-github/v53/github"
)

// fakePulls records each call as one line, so a test compares the whole
// conversation with the API.
type fakePulls struct {
	open    []*github.PullRequest
	listErr error
	label   error
	calls   []string
}

func (f *fakePulls) List(_ context.Context, owner, repo string, opts *github.PullRequestListOptions) ([]*github.PullRequest, *github.Response, error) {
	f.calls = append(f.calls, "list "+owner+"/"+repo+" "+opts.State+" "+opts.Head+" -> "+opts.Base)
	return f.open, nil, f.listErr
}

func (f *fakePulls) Create(_ context.Context, _, _ string, pull *github.NewPullRequest) (*github.PullRequest, *github.Response, error) {
	f.calls = append(f.calls, "create "+pull.GetHead()+" -> "+pull.GetBase()+" "+pull.GetTitle()+" | "+pull.GetBody())
	return &github.PullRequest{Number: github.Int(7)}, nil, nil
}

func (f *fakePulls) RequestReviewers(_ context.Context, _, _ string, _ int, r github.ReviewersRequest) (*github.PullRequest, *github.Response, error) {
	f.calls = append(f.calls, "reviewers", "  users "+strings.Join(r.Reviewers, ","), "  teams "+strings.Join(r.TeamReviewers, ","))
	return nil, nil, nil
}

func (f *fakePulls) AddLabelsToIssue(_ context.Context, _, _ string, _ int, labels []string) ([]*github.Label, *github.Response, error) {
	f.calls = append(f.calls, "labels "+strings.Join(labels, ","))
	return nil, nil, f.label
}

func TestPullRequestOpen(t *testing.T) {
	const list = "list tigera/calico open tigera:build-v3.33.1 -> release-v3.33"
	created := "create build-v3.33.1 -> release-v3.33 build: v3.33.1 release | Release-Branch: release-v3.33"
	finish := []string{"labels a,b", "reviewers", "  users someone", "  teams release-team"}
	existing := []*github.PullRequest{{Number: github.Int(5)}}
	for _, tc := range []struct {
		name    string
		fake    fakePulls
		local   bool
		noToken bool
		want    []string
		wantErr string
	}{
		{name: "local without a token calls nothing", local: true, noToken: true},
		{name: "opens a PR and finishes it", want: append([]string{list, created}, finish...)},
		{name: "reuses an open PR and finishes it", fake: fakePulls{open: existing}, want: append([]string{list}, finish...)},
		{name: "local looks up but opens nothing", local: true, want: []string{list}},
		{name: "local with an open PR changes nothing", local: true, fake: fakePulls{open: existing}, want: []string{list}},
		{name: "a failed lookup stops", fake: fakePulls{listErr: errors.New("boom")}, want: []string{list}, wantErr: "list open PRs"},
		{
			name:    "a failed label stops before the reviewer request",
			fake:    fakePulls{label: errors.New("boom")},
			want:    []string{list, created, "labels a,b"},
			wantErr: "label",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, key := range TokenEnvVars {
				t.Setenv(key, "")
			}
			cli := "tok"
			if tc.noToken {
				cli = ""
			}
			f := tc.fake
			p := PullRequest{
				Runner:    fakeGH(cli),
				Repo:      Repo{Owner: "tigera", Name: "calico"},
				Base:      "release-v3.33",
				Head:      "build-v3.33.1",
				Title:     "build: v3.33.1 release",
				Body:      "Release-Branch: release-v3.33",
				Reviewers: []string{"someone", "tigera/release-team"},
				Labels:    []string{"a", "b"},
				Local:     tc.local,
				pulls:     &f,
				issues:    &f,
			}
			err := p.Open()
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
			if diff := cmp.Diff(tc.want, f.calls); diff != "" {
				t.Errorf("API calls (-want +got):\n%s", diff)
			}
		})
	}
}

func TestPullRequestOpened(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fake    fakePulls
		wantErr string
	}{
		{name: "an open PR", fake: fakePulls{open: []*github.PullRequest{{Number: github.Int(5)}}}},
		{name: "no open PR", wantErr: "no open PR"},
		{name: "a failed lookup", fake: fakePulls{listErr: errors.New("boom")}, wantErr: "list open PRs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fake
			p := PullRequest{Repo: Repo{Owner: "tigera", Name: "calico"}, Base: "release-v3.33", Head: "build-v3.33.1", pulls: &f, issues: &f}
			err := p.Opened()
			if tc.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}
