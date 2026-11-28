// Copyright (c) 2021-2026 Tigera, Inc. All rights reserved.

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
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/sirupsen/logrus"

	"github.com/projectcalico/calico/release/internal/archives"
	"github.com/projectcalico/calico/release/internal/binaries"
	"github.com/projectcalico/calico/release/internal/branch"
	"github.com/projectcalico/calico/release/internal/charts"
	"github.com/projectcalico/calico/release/internal/command"
	"github.com/projectcalico/calico/release/internal/defaults"
	"github.com/projectcalico/calico/release/internal/distribution"
	"github.com/projectcalico/calico/release/internal/fsutil"
	"github.com/projectcalico/calico/release/internal/github"
	"github.com/projectcalico/calico/release/internal/hashreleaseserver"
	"github.com/projectcalico/calico/release/internal/images"
	"github.com/projectcalico/calico/release/internal/imagescanner"
	"github.com/projectcalico/calico/release/internal/manifests"
	"github.com/projectcalico/calico/release/internal/operator"
	"github.com/projectcalico/calico/release/internal/outputs"
	"github.com/projectcalico/calico/release/internal/plan"
	"github.com/projectcalico/calico/release/internal/registry"
	"github.com/projectcalico/calico/release/internal/version"
)

// Global configuration for releases.
const (
	binDir = "bin"

	gitTrailerReleaseBranch = "Release-branch"
)

const (
	prepBuildStep   = "prep-build"
	prepPublishStep = "prep-publish"
	prepReleaseStep = "prep-release"
)

var (
	releaseBuildSteps     = []string{branch.CreateTagStep}
	hashreleaseBuildSteps = []string{outputs.ReleaseNotesStep}
	sharedBuildSteps      = []string{
		operator.Step, images.Step, binaries.Step, charts.Step, manifests.Step,
		archives.WindowsStep, archives.Step,
	}
	sharedPublishSteps      = []string{operator.Step, images.Step, charts.Step, outputs.MetadataStep, outputs.ChecksumStep}
	releasePublishSteps     = []string{branch.PushTagStep, github.ReleaseStep, charts.IndexStep}
	hashreleasePublishSteps = []string{distribution.HashreleasePublishStep}

	// BuildSteps are the steps a release build can skip, in the order they run.
	BuildSteps = func() []string { return slices.Concat(releaseBuildSteps, sharedBuildSteps) }

	// HashreleaseBuildSteps are the steps a hashrelease build can skip, in the
	// order they run.
	HashreleaseBuildSteps = func() []string {
		return slices.Concat(hashreleaseBuildSteps, sharedBuildSteps)
	}

	// PublishSteps are the steps a release publish can skip, in the order they run.
	PublishSteps = func() []string { return slices.Concat(sharedPublishSteps, releasePublishSteps) }

	// HashreleasePublishSteps are the steps a hashrelease publish can skip, in the
	// order they run.
	HashreleasePublishSteps = func() []string {
		return slices.Concat(sharedPublishSteps, hashreleasePublishSteps)
	}

	// CutSteps are the steps a branch cut can skip, in the order they run.
	CutSteps = branch.CutSteps

	// PrepSteps are the steps a release prep can skip, in the order they run.
	PrepSteps = func() []string {
		return slices.Concat([]string{outputs.ReleaseNotesStep}, branch.PrepSteps(), []string{github.PRStep})
	}
)

var (
	// Default defaultRegistries to which all release images are pushed.
	defaultRegistries = registry.DefaultCalicoRegistries

	calicoRepo = github.Repo{Owner: defaults.ProjectCalicoOrg, Name: defaults.CalicoRepoName}

	defaultBranch = defaults.Branch

	branchTagTarget = "retag-build-images-with-registries push-images-to-registries push-manifests"

	// Windows ships one manifest, so its branch tag is a registry-side copy.
	windowsBranchTagTarget = "retag-windows-image-with-registries"

	windowsBranchTagDirs = slices.Clone(images.WindowsReleaseDirs)
)

func NewManager(opts ...Option) (*CalicoManager, error) {
	// Configure defaults here.
	b := &CalicoManager{
		runner:           command.RealRunner,
		productCode:      defaults.ProductCode,
		validate:         true,
		validateBranch:   true,
		manifests:        true,
		images:           true,
		archiveImages:    true,
		binaries:         true,
		ocpBundle:        true,
		tarball:          true,
		windowsArchive:   true,
		helmCharts:       true,
		helmIndex:        true,
		e2eBinaries:      true,
		releaseNotes:     false,
		gitRef:           true,
		githubRelease:    true,
		imageRegistries:  defaultRegistries,
		helmRepoURL:      charts.DefaultRepoURL,
		helmRegistries:   registry.DefaultHelmRegistries,
		operatorRegistry: registry.DefaultOperatorRegistry,
		operatorImage:    registry.OperatorImage,
	}

	for _, o := range opts {
		if err := o(b); err != nil {
			return nil, fmt.Errorf("applying option: %w", err)
		}
	}
	if b.repoRoot == "" {
		return nil, fmt.Errorf("no repo root specified")
	}
	logrus.WithField("repoRoot", b.repoRoot).Info("Using repo root")
	if b.logsDir != "" {
		logrus.WithField("logsDir", b.logsDir).Info("Per-step logs enabled")
	}

	// Skipping images means using the published ones, which the images step resolves.
	if b.planOptions.Skip[images.Step] {
		b.images = false
		b.planOptions.Skip = maps.Clone(b.planOptions.Skip)
		delete(b.planOptions.Skip, images.Step)
	}
	return b, nil
}

type CalicoManager struct {
	// Allow specification of command runner so it can be overridden in tests.
	runner command.CommandRunner

	// The product code for the release.
	productCode string

	// The abs path of the root of the repository.
	repoRoot string

	// isHashRelease is a flag to indicate that we should build a hashrelease.
	isHashRelease bool

	ci bool

	// archiveImages controls whether we should archive container images in release tarball.
	archiveImages bool

	// validate is a flag to indicate that we should skip pre-release validation.
	validate bool

	// validateBranch is a flag to indicate if branch validation should fail the run.
	validateBranch bool

	// calicoVersion is the version of calico to release.
	calicoVersion version.Version

	// chartVersion is the version of the helm chart to build.
	chartVersion string

	// operator variables
	operatorImage    string
	operatorRegistry string
	operatorVersion  string

	// imageReleaseDirs limits image building and publishing to these
	// directories. Empty means all of them.
	imageReleaseDirs []string

	resolveDigest registry.DigestResolver

	// outputDir is the directory to which we should write release artifacts, and from
	// which we should read them for publishing.
	outputDir string

	recordsDir string

	// tmpDir is the directory to which we should write temporary files.
	tmpDir string

	// logsDir is where per-step build/publish logs are written. Each long-running
	// make invocation gets its own file under <logsDir>/<phase>/<component>.log so
	// that timing and failures for an individual component are easy to dig into
	// after a run, and the directory can be uploaded as a CI artifact.
	logsDir string

	gitRef        bool
	githubRelease bool
	draftRelease  bool
	awsProfile    string
	s3Bucket      string

	// imageRegistries is the list of imageRegistries to which we should publish images.
	imageRegistries []string

	// helmRegistries is the list of OCI-based registries to which we should publish charts.
	helmRegistries []string

	// helmRepoURL is the URL of the helm chart repository.
	helmRepoURL string

	repo github.Repo

	// mainBranch is the default branch for the repo
	// It is also where a new release branch is cut from.
	mainBranch string

	// devTagIdentifier is the suffix used to mark a tag as a development tag.
	devTagIdentifier string

	// releaseBranchPrefix is the prefix for the release branch.
	releaseBranchPrefix string

	// beforePrep runs on the base branch, after the prep checks pass.
	beforePrep beforePrepDriver

	// architectures is the list of architectures for which we should build images.
	// If empty, we build for all.
	architectures []string

	// hashrelease configuration.
	publishHashrelease bool
	hashrelease        hashreleaseserver.Hashrelease
	hashreleaseConfig  hashreleaseserver.Config

	// image scanning configuration.
	imageScanning       bool
	imageScanningConfig imagescanner.Config
	components          map[string]registry.Component
	// Resolved before the product images; a product adds its own.
	imageDependencies []images.Dependency

	// Unified step flags.
	manifests      bool
	images         bool
	binaries       bool
	ocpBundle      bool
	windowsArchive bool
	tarball        bool
	helmCharts     bool
	helmIndex      bool
	e2eBinaries    bool
	releaseNotes   bool

	planOptions  plan.Options
	buildNodes   func() []plan.Node
	publishNodes func() []plan.Node
	operator     operator.Driver

	retagImages  bool
	fromRegistry string
	fromTag      string
}

func (r *CalicoManager) canBuildValidation() error {
	var errStack error
	if r.calicoVersion.String() == "" {
		errStack = errors.Join(errStack, fmt.Errorf("no calico version specified"))
	}
	if r.outputDir == "" {
		errStack = errors.Join(errStack, fmt.Errorf("no output directory specified"))
	}

	if r.operatorVersion == "" {
		errStack = errors.Join(errStack, fmt.Errorf("no operator version specified"))
	}
	if (r.images || r.archiveImages) && len(r.imageRegistries) == 0 {
		errStack = errors.Join(errStack, fmt.Errorf("no image registries specified"))
	}
	if r.isHashRelease && r.ocpBundle && !r.manifests {
		errStack = errors.Join(errStack, fmt.Errorf("cannot build OCP bundle without manifests; set --manifests to 'true'"))
	}
	if errStack != nil {
		return errStack
	}
	logrus.WithField("version", r.calicoVersion.String()).Info("Using product version")
	logrus.WithField("operatorVersion", r.operatorVersion).Info("Using operator version")
	logrus.WithField("registries", r.imageRegistries).Info("Using image registries for release")
	if r.isHashRelease {
		return r.PreHashreleaseValidate()
	}
	return r.PreReleaseValidate()
}

func (r *CalicoManager) Build() error {
	nodes := r.buildPlan
	if r.buildNodes != nil {
		nodes = r.buildNodes
	}
	return r.runPlan(nodes())
}

func (r *CalicoManager) createDirs() error {
	var errStack []error
	if err := os.MkdirAll(r.uploadDir(), fsutil.DirPerms); err != nil {
		errStack = append(errStack, fmt.Errorf("failed to create output dir: %s", err))
	}
	if r.tmpDir != "" {
		if err := os.MkdirAll(r.tmpDir, fsutil.DirPerms); err != nil {
			errStack = append(errStack, fmt.Errorf("failed to create temp dir: %s", err))
		}
	}
	return errors.Join(errStack...)
}

func (r *CalicoManager) mustBuildValidation() error {
	var errs []error
	if r.outputDir == "" {
		errs = append(errs, fmt.Errorf("no output directory specified"))
	}
	if r.recordsDir == "" {
		errs = append(errs, fmt.Errorf("no records directory specified"))
	}
	if !r.isHashRelease || r.releaseNotes {
		if err := r.repo.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if r.isHashRelease {
		return errors.Join(errs...)
	}

	out, err := command.Git(r.runner).In(r.repoRoot).Version(true)
	if err != nil {
		errs = append(errs, fmt.Errorf("determine git version: %w", err))
		return errors.Join(errs...)
	}
	gitVer, err := version.New(strings.TrimSpace(out))
	if err != nil {
		errs = append(errs, fmt.Errorf("determine git version: %w", err))
		return errors.Join(errs...)
	}
	ver, err := version.DetermineReleaseVersion(gitVer, r.devTagIdentifier)
	if err != nil {
		errs = append(errs, fmt.Errorf("release version: %w", err))
		return errors.Join(errs...)
	}
	if ver.String() != r.calicoVersion.String() {
		errs = append(errs, fmt.Errorf("manifests are at %s but git says this release is %s: run release prep first", r.calicoVersion, ver))
	}

	return errors.Join(errs...)
}

func (r *CalicoManager) buildPlan() []plan.Node {
	nodes := []plan.Node{
		{
			Name: prepBuildStep,
			Validate: func() error {
				if err := r.mustBuildValidation(); err != nil {
					return err
				}
				if !r.validate {
					return nil
				}
				return r.canBuildValidation()
			},
			Do: r.createDirs,
		},
	}
	if r.isHashRelease {
		nodes = append(nodes, plan.Node{
			Name: outputs.ReleaseNotesStep,
			When: func() bool { return r.releaseNotes && r.repo.Is(calicoRepo) },
			Do: func() error {
				ver, err := version.DetermineReleaseVersion(r.calicoVersion, r.devTagIdentifier)
				if err != nil {
					return fmt.Errorf("determine release version: %w", err)
				}
				return r.buildReleaseNotes(filepath.Join(r.outputDir, outputs.ReleaseNotesDir), ver)
			},
		})
	} else {
		nodes = append(nodes, r.createTagNode())
	}
	nodes = append(
		nodes,
		r.operatorNode(nil),
		plan.Node{Name: images.Step, When: func() bool { return r.images }, Do: r.buildContainerImages},
		plan.Node{
			Name: binaries.Step,
			When: func() bool { return r.binaries || (r.e2eBinaries && r.isHashRelease) },
			Do:   r.buildBinaries,
			Done: r.binariesBuilt,
		},
		plan.Node{
			Name: charts.Step,
			When: func() bool { return r.helmCharts },
			Do:   r.BuildHelm,
			Done: func() error { return charts.Built(r.chart()) },
		},
		plan.Node{
			Name: manifests.Step,
			When: func() bool {
				return (r.isHashRelease && r.manifests) || r.ocpBundle
			},
			Do:   r.buildManifests,
			Done: func() error { return manifests.Built(r.manifestsRoot(), r.uploadDir(), r.ocpBundle) },
		},
		plan.Node{
			Name: archives.WindowsStep,
			When: func() bool { return r.windowsArchive },
			Do:   r.buildWindowsArchive,
			Done: func() error { return archives.WindowsBuilt(r.archive()) },
		},
		plan.Node{
			Name:     archives.Step,
			When:     func() bool { return r.tarball },
			Requires: r.releaseArchiveRequires(),
			Validate: func() error {
				_, err := r.archiveSources()
				return err
			},
			Do:   r.buildReleaseTar,
			Done: func() error { return archives.Built(r.archive()) },
		},
	)
	return nodes
}

func (r *CalicoManager) createTagNode() plan.Node {
	return plan.Node{
		Name: branch.CreateTagStep,
		Do: func() error {
			ver := r.calicoVersion.String()
			return branch.CreateTag(command.Git(r.runner).In(r.repoRoot), ver, branch.TagMessage("Release "+ver))
		},
		Rollback: func() error {
			return branch.DeleteTag(command.Git(r.runner).In(r.repoRoot), r.calicoVersion.String())
		},
	}
}

func (r *CalicoManager) operatorNode(validateFn func() error) plan.Node {
	return plan.Node{
		Name:     operator.Step,
		Validate: validateFn,
		Do:       func() error { return r.operator.Do() },
		When:     func() bool { return r.operator.Do != nil },
		Done:     r.operator.Done,
	}
}

// Unbuilt images are pulled instead, and a release stages the checked-in
// manifests, so neither needs its step.
func (r *CalicoManager) releaseArchiveRequires() []string {
	var req []string
	if r.archiveImages && r.images {
		req = append(req, images.Step)
	}
	if r.binaries {
		req = append(req, binaries.Step)
	}
	if r.manifests && r.isHashRelease {
		req = append(req, manifests.Step)
	}
	return req
}

func (r *CalicoManager) BuildMetadata(dir string) error {
	d := outputs.Digests{Resolve: r.digestResolver(), Require: !r.isHashRelease}
	released := r.releasedComponents()
	op, hasOperator := released[r.operatorImage]
	delete(released, r.operatorImage)
	components, err := images.Metadata(r.recordsDir, released, d)
	if err != nil {
		return fmt.Errorf("images metadata: %w", err)
	}
	if hasOperator {
		if components[r.operatorImage], err = operator.Metadata(r.recordsDir, op, d); err != nil {
			return fmt.Errorf("operator metadata: %w", err)
		}
	}
	charts, err := r.chartsMetadata(d)
	if err != nil {
		return err
	}
	artifacts, err := r.artifacts(dir)
	if err != nil {
		return err
	}
	source, err := r.sourceMetadata()
	if err != nil {
		return err
	}
	return outputs.BuildMetadata(&outputs.Metadata{
		Version:         r.calicoVersion.String(),
		OperatorVersion: r.operatorVersion,
		ChartVersion:    r.chart().Version(),
		Source:          source,
		Charts:          charts,
		Artifacts:       artifacts,
		Components:      components,
	}, dir)
}

func (r *CalicoManager) metadataRequires() []string {
	req := []string{images.Step}
	if r.operator.Do != nil {
		req = append(req, operator.Step)
	}
	if r.helmCharts {
		req = append(req, charts.Step)
	}
	return req
}

// A detached HEAD, as CI often checks out, has no branch to record.
func (r *CalicoManager) sourceMetadata() (outputs.Source, error) {
	commit, err := r.git("rev-parse", "HEAD")
	if err != nil {
		return outputs.Source{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	branch, err := r.git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return outputs.Source{}, fmt.Errorf("determining branch: %w", err)
	}
	src := outputs.Source{
		Repository: r.repo.FullName(),
		Commit:     strings.TrimSpace(commit),
	}
	if b := strings.TrimSpace(branch); b != "HEAD" {
		src.Branch = b
	}
	if !r.isHashRelease && r.gitRef {
		src.Tag = r.calicoVersion.String()
	}
	return src, nil
}

// artifacts describes the files in dir that users download, at the URL the
// release serves them from.
func (r *CalicoManager) artifacts(dir string) ([]outputs.Artifact, error) {
	var base string
	switch {
	case r.isHashRelease && r.publishHashrelease:
		base = r.hashrelease.URL()
	case !r.isHashRelease && r.githubRelease:
		base = r.repo.DownloadURL(r.calicoVersion.String())
	default:
		return nil, nil
	}
	return outputs.Artifacts(dir, base, github.AssetsFilter...)
}

// Each chart is recorded at the first registry it was published to.
func (r *CalicoManager) chartsMetadata(d outputs.Digests) (*outputs.Charts, error) {
	if !r.helmCharts {
		return nil, nil
	}
	if len(r.helmRegistries) == 0 {
		return nil, fmt.Errorf("no helm chart registries specified")
	}
	chart := r.chart()
	at := charts.Published{Registry: r.helmRegistries[0], DownloadURL: r.chartsURL()}
	switch {
	case !r.helmIndex:
	case r.isHashRelease:
		index, err := url.JoinPath(r.hashrelease.URL(), charts.DirName)
		if err != nil {
			return nil, fmt.Errorf("hashrelease charts index: %w", err)
		}
		at.Index = index
	default:
		at.Index = chart.Repo()
	}
	return charts.Metadata(r.recordsDir, chart, at, d)
}

// Fetch the registry from the calicoctl manifest file.
// For hashrelease, it looks in the hashrelease source directory.
func (r *CalicoManager) getRegistryFromManifests() (string, error) {
	root := r.repoRoot
	if r.isHashRelease {
		if _, err := os.Stat(filepath.Join(manifests.Dir(r.hashrelease.Source), manifests.RegistryFile)); err != nil {
			// if the file does not exist, fall back to the default image registry.
			return r.imageRegistries[0], nil
		}
		root = r.hashrelease.Source
	}
	return manifests.Registry(root)
}

// Release branch and version patterns. A fork with its own branch or version
// scheme overrides them from an init function.
var (
	// releaseBranchTail matches what follows "<prefix>-" on a release branch.
	releaseBranchTail = `v\d+\.\d+(?:-\d+)?`

	// releaseVersionRe and operatorReleaseVersionRe match the versions a release may carry.
	releaseVersionRe         = regexp.MustCompile(`^v\d+\.\d+\.\d+(-\d+\.\d+)?$`)
	operatorReleaseVersionRe = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
)

func (r *CalicoManager) PreHashreleaseValidate() error {
	var errStack error
	if err := r.checkBranch(fmt.Sprintf(`^(%s|%s-%s)$`, defaults.Branch, r.releaseBranchPrefix, releaseBranchTail), "hashrelease branch"); err != nil {
		errStack = errors.Join(errStack, err)
	}
	if dirty, err := command.Git(r.runner).In(r.repoRoot).Dirty(); err != nil {
		return fmt.Errorf("check if git is dirty: %w", err)
	} else if dirty {
		errStack = errors.Join(errStack, fmt.Errorf("there are uncommitted changes in the repository, please commit or stash them"))
	}
	return errStack
}

func (r *CalicoManager) checkBranch(match, desc string) error {
	if !r.validateBranch {
		logrus.Warn("Skipping branch check because validateBranch is false")
		return nil
	}
	branch, err := command.Git(r.runner).In(r.repoRoot).Branch()
	if err != nil {
		return fmt.Errorf("get current branch: %w", err)
	}
	re, err := regexp.Compile(match)
	if err != nil {
		return fmt.Errorf("regexp compile: %w", err)
	}
	if !re.MatchString(branch) {
		return fmt.Errorf("check branch: current branch (%s) is not a %s", branch, desc)
	}
	return nil
}

func (r *CalicoManager) PreReleaseValidate() error {
	var errs []error
	if err := r.assertReleaseNotesPresent(r.calicoVersion.String()); err != nil {
		errs = append(errs, err)
	}
	// Check that we are on a release branch
	if err := r.checkBranch(fmt.Sprintf(`^(%s-%s|build-v\d+\.\d+\.\d+)$`, r.releaseBranchPrefix, releaseBranchTail), "release branch"); err != nil {
		errs = append(errs, err)
	}
	// Check that we're not already on a git tag.
	if out, err := r.git("describe", "--exact-match", "--tags", "--match", command.VersionTags, "HEAD"); err == nil {
		errs = append(errs, fmt.Errorf("already on a tag (%s), refusing to create release", out))
	}

	// Check that the repository is not a shallow clone. We need correct history.
	if out, err := r.git("rev-parse", "--is-shallow-repository"); err != nil {
		errs = append(errs, fmt.Errorf("check for a shallow clone: %w", err))
	} else if strings.TrimSpace(out) == "true" {
		errs = append(errs, fmt.Errorf("attempt to release from a shallow clone is not possible"))
	}

	// Assert that manifests are using the correct version.
	if err := manifests.AssertVersions(r.manifestValues(), manifests.WithRunner(r.runner)); err != nil {
		errs = append(errs, err)
	}
	if err := r.releasePrereqs(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (r *CalicoManager) BuildHelm() error {
	chart := r.chart()
	opts := []charts.BuildOption{charts.WithRunner(r.runner)}
	if r.helmIndex {
		opts = append(opts, charts.WithIndex(r.chartsURL(), charts.IndexDir(r.uploadDir()), r.tmpDir))
	}
	if r.isHashRelease {
		opts = append(opts, charts.WithModifiedValues(charts.ValueEditsFor(r.calicoVersion.String(), r.imageRegistries[0], r.operatorImage, r.operatorVersion, r.operatorRegistry)))
	}
	return charts.Build(chart, opts...)
}

func (r *CalicoManager) chartsURL() string {
	if r.isHashRelease {
		return r.hashrelease.URL()
	}
	return charts.ChartsURL(r.chart())
}

// chart identifies this release's charts to the charts package.
func (r *CalicoManager) chart() charts.Chart {
	return charts.Chart{
		RepoRoot:       r.repoRoot,
		ProductVersion: r.calicoVersion.String(),
		ChartVersion:   r.chartVersion,
		Names:          charts.All(),
		BaseDir:        charts.OutputDir(r.uploadDir()),
		RepoURL:        r.helmRepoURL,
	}
}

// modifyHelmChartsValues rewrites the chart values in the tree.
func (r *CalicoManager) modifyHelmChartsValues(calicoVersion, operatorVersion string) error {
	return charts.ModifyValues(charts.Values{
		RepoRoot: r.repoRoot,
		Edits:    charts.ValueEditsFor(calicoVersion, r.imageRegistries[0], r.operatorImage, operatorVersion, r.operatorRegistry),
	}, charts.WithRunner(r.runner))
}

func (r *CalicoManager) hashreleaseUpload() distribution.Upload {
	return distribution.Upload{
		Name:     distribution.HashreleasePublishStep,
		Source:   r.uploadDir(),
		Skip:     !r.publishHashrelease,
		Requires: []string{outputs.ChecksumStep, outputs.MetadataStep},
		Handler: distribution.HashreleaseServer{
			Release:     &r.hashrelease,
			Config:      &r.hashreleaseConfig,
			ProductCode: r.productCode,
			DryRun:      r.planOptions.Local,
		},
	}
}

func (r *CalicoManager) releaseUploads() []distribution.Upload {
	ver := r.calicoVersion.String()
	vpp := github.Link{
		Text: "VPP data plane release information",
		URL:  github.Repo{Owner: "projectcalico", Name: "vpp-dataplane"}.BlobURL("master", "RELEASE_NOTES.md"),
	}
	notes := []string{vpp.String()}
	// notes = append(notes, "[Upgrade guide]("+docs.Site{Version: r.calicoVersion}.URL("operations", "upgrading")+")")
	if r.helmCharts && len(r.helmRegistries) > 0 {
		var refs []string
		for _, chart := range charts.All() {
			refs = append(refs, "`oci://"+r.helmRegistries[0]+"/"+chart+"`")
		}
		notes = append(notes, "All Helm charts are also in an OCI registry: "+strings.Join(refs, ", ")+".")
	}
	return []distribution.Upload{
		branch.TagUpload(branch.PushTagOptions{
			Tag:      ver,
			Remote:   r.repo.Remote,
			RepoRoot: r.repoRoot,
			Skip:     !r.gitRef,
			Local:    r.planOptions.Local,
			Runner:   r.runner,
		}),
		github.ReleaseUpload(github.ReleaseOptions{
			Repo:    r.repo,
			Version: r.calicoVersion,
			Source:  r.uploadDir(),
			Notes: github.ReleaseNotes{
				Link: github.Link{
					Text: "on GitHub",
					URL:  r.repo.BlobURL(version.NewReleaseBranch(r.releaseBranchPrefix, r.calicoVersion).String(), outputs.ReleaseNoteFilePath("", ver)),
				},
				Artifacts: []github.ReleaseArtifact{
					{Name: archives.ArchiveFileName(r.archive()), Description: "container images, binaries, and kubernetes manifests."},
					{Name: archives.WindowsFileName(ver), Description: "Calico for Windows."},
					{Name: charts.FileName(charts.TigeraOperatorChart, ver), Description: "Calico Helm 3 chart."},
					{Name: charts.FileName(charts.ProjectCalicoV1CRDsChart, ver), Description: "Calico crd.projectcalico.org/v1 CRD chart."},
					{Name: charts.FileName(charts.ProjectCalicoV3CRDsChart, ver), Description: "Calico projectcalico.org/v3 CRD chart (tech-preview)."},
					{Name: manifests.OCPBundleFileName, Description: "Manifest bundle for OpenShift."},
				},
				Notes: notes,
			},
			// A release is cut from the tag, so no git refs means no release.
			Skip:   !r.githubRelease || !r.gitRef,
			Draft:  r.draftRelease,
			DryRun: r.planOptions.Local,
			Runner: r.runner,
		}),
	}
}

func (r *CalicoManager) PublishRelease() error {
	nodes := r.publishPlan
	if r.publishNodes != nil {
		nodes = r.publishNodes
	}
	return r.runPlan(nodes())
}

func (r *CalicoManager) publishPlan() []plan.Node {
	var validateFn func() error
	if r.validate {
		validateFn = func() error {
			return images.Built(r.runner, r.operatorVersion, []string{r.operatorRegistry}, []string{r.operatorImage})
		}
	}
	nodes := []plan.Node{
		{Name: prepPublishStep, Validate: r.publishPrereqs},
		r.operatorNode(validateFn),
	}
	for _, u := range r.uploads() {
		nodes = append(nodes, distribution.Node(u, distribution.WithRunner(r.runner)))
	}
	return nodes
}

func (r *CalicoManager) uploads() []distribution.Upload {
	// The registries go first: metadata records the digests they produce
	uploads := []distribution.Upload{
		// images are never off: a skip resolves the earlier run's digests from the registry instead.
		{
			Handler:  distribution.Publisher{Kind: images.Step, Action: r.imagesAction(), Check: r.checkImagesBuilt},
			Requires: r.beforeImagesPublish(),
		},
		{
			Handler: distribution.Publisher{Kind: charts.Step, Action: r.publishHelmCharts},
			Skip:    !r.helmCharts,
			Done: func() error {
				return charts.Pushed(r.chart(), r.helmRegistries, r.recordsDir,
					charts.WithRunner(r.runner), charts.WithResolver(r.digestResolver()))
			},
		},
	}
	uploads = append(
		uploads,
		outputs.MetadataUpload(r.uploadDir(), r.buildMetadata, r.metadataRequires()...),
		outputs.ChecksumUpload(r.runner, r.uploadDir(), github.AssetsFilter...),
	)
	if r.isHashRelease {
		return append(uploads, r.hashreleaseUpload())
	}
	uploads = append(uploads, r.releaseUploads()...)
	return append(uploads, charts.IndexUpload(charts.IndexOptions{
		OutputDir: r.uploadDir(),
		Handler:   distribution.S3Handler(r.awsProfile, r.s3Bucket, r.planOptions.Local),
		Skip:      !r.helmCharts || !r.helmIndex,
	}))
}

// The images scan sends the operator to ISS, so it must be published first.
func (r *CalicoManager) beforeImagesPublish() []string {
	if r.operator.Do == nil {
		return nil
	}
	return []string{operator.Step}
}

func (r *CalicoManager) buildMetadata() error {
	return r.BuildMetadata(r.uploadDir())
}

// Check general prerequisites for cutting and publishing a release.
func (r *CalicoManager) releasePrereqs() error {
	// Check that we're not on the master branch. We never cut releases from master.
	if branch, err := r.determineBranch(); err != nil {
		return fmt.Errorf("failed to determine branch: %s", err)
	} else if branch == defaultBranch {
		return fmt.Errorf("cannot cut release from branch: %s", branch)
	}

	// If we are releasing to projectcalico/calico, make sure we are releasing to the default registries.
	if r.repo.Is(calicoRepo) {
		if !reflect.DeepEqual(r.imageRegistries, defaultRegistries) {
			return fmt.Errorf("image registries cannot be different from default registries for a release")
		}
	}

	if _, err := branch.TagAtHEAD(command.Git(r.runner).In(r.repoRoot), r.calicoVersion.String()); err != nil {
		return err
	}

	return nil
}

func (r *CalicoManager) componentImages() map[string]string {
	images := map[string]string{}
	for name, component := range r.releasedComponents() {
		if component.Image != "" {
			images[name] = component.String()
		}
	}
	return images
}

func (r *CalicoManager) releasedComponents() map[string]registry.Component {
	released := make(map[string]registry.Component, len(r.components))
	for name, component := range r.components {
		if name == r.operatorImage {
			// A registry flag can move the run's operator off the pin.
			component = r.operatorComponent()
		}
		if component.Image != "" && component.Registry == "" {
			component.Registry = r.imageRegistries[0]
		}
		released[name] = component
	}
	return released
}

// Check that the environment has the necessary prereqs for publishing hashrelease
func (r *CalicoManager) hashreleasePrereqs() error {
	if r.publishHashrelease && !r.hashreleaseConfig.Valid() {
		return fmt.Errorf("missing hashrelease server configuration")
	}
	return nil
}

// checkImagesBuilt reads the image list from make, so it runs only when the
// images publish.
func (r *CalicoManager) checkImagesBuilt() error {
	if !r.validate || !r.images || r.retagImages {
		return nil
	}
	dirs := images.VariantDirs(images.NarrowVariants(images.PublishVariants, r.imageReleaseDirs))
	names, err := images.BuildImages(r.runner, r.repoRoot, dirs...)
	if err != nil {
		return err
	}
	return images.Built(r.runner, r.calicoVersion.String(), r.imageRegistries, names)
}

// Prerequisites specific to publishing a release.
func (r *CalicoManager) publishPrereqs() error {
	if err := r.mustPublishValidation(); err != nil {
		return err
	}
	if !r.validate {
		logrus.Warn("Skipping pre-publish validation")
		return nil
	}
	var errStack error
	if r.calicoVersion.String() == "" {
		errStack = errors.Join(errStack, fmt.Errorf("no calico version specified"))
	}
	if r.images && len(r.imageRegistries) == 0 {
		errStack = errors.Join(errStack, fmt.Errorf("no image registries specified"))
	}
	if r.helmCharts {
		if len(r.helmRegistries) == 0 {
			errStack = errors.Join(errStack, fmt.Errorf("no helm chart registries specified"))
		}
		if !r.isHashRelease && r.s3Bucket == "" {
			errStack = errors.Join(errStack, fmt.Errorf("no S3 bucket specified for pushing helm index"))
		}
	}
	if dirty, err := command.Git(r.runner).In(r.repoRoot).Dirty(); dirty || err != nil {
		errStack = errors.Join(errStack, fmt.Errorf("there are uncommitted changes in the repository, please commit or stash them before publishing the release"))
	}
	if errStack != nil {
		return errStack
	}
	logrus.WithField("version", r.calicoVersion.String()).Info("Using product version")
	logrus.WithField("registries", r.imageRegistries).Info("Using image registries for publishing")
	if r.isHashRelease {
		return r.hashreleasePrereqs()
	}
	return r.releasePrereqs()
}

func (r *CalicoManager) mustPublishValidation() error {
	var errs error
	if r.outputDir == "" {
		errs = errors.Join(errs, fmt.Errorf("no output directory specified"))
	}
	if r.recordsDir == "" {
		errs = errors.Join(errs, fmt.Errorf("no records directory specified"))
	}
	if err := r.repo.Validate(); err != nil {
		errs = errors.Join(errs, err)
	}
	return errs
}

func (r *CalicoManager) archive() archives.Archive {
	return archives.Archive{
		RepoRoot:        r.repoRoot,
		Version:         r.calicoVersion.String(),
		OperatorVersion: r.operatorVersion,
		OutputDir:       r.uploadDir(),
	}
}

func (r *CalicoManager) buildWindowsArchive() error {
	return archives.BuildWindows(
		r.archive(),
		archives.WithRunner(r.runner),
		archives.WithLogsDir(r.logsDir),
	)
}

// buildManifests regenerates manifests for pinned calico/operator versions and
// builds the manifest-derived OCP bundle.
func (r *CalicoManager) buildManifests() error {
	// Only a hashrelease regenerates: a release ships the checked-in manifests.
	generate := r.isHashRelease && r.manifests
	opts := []manifests.BuildOption{manifests.WithRunner(r.runner), manifests.WithLogsDir(r.logsDir)}
	if r.isHashRelease {
		// A release ships the manifests in the archive instead.
		opts = append(opts, manifests.WithCollect())
	}
	return manifests.Build(r.manifestValues(), generate, r.ocpBundle, opts...)
}

func (r *CalicoManager) manifestValues() manifests.Manifests {
	return manifests.Manifests{
		RepoRoot:  r.repoRoot,
		Version:   r.calicoVersion.String(),
		Operator:  r.operatorComponent(),
		Registry:  r.imageRegistries[0],
		OutputDir: r.uploadDir(),
	}
}

func (r *CalicoManager) operatorComponent() registry.Component {
	return registry.Component{
		Version:  r.operatorVersion,
		Image:    r.operatorImage,
		Registry: r.operatorRegistry,
	}
}

// Compared against the manifests, not the default registries: those can
// disagree, and the checked-in one may be a registry a release must not use.
// Validated before any step runs; see publishPrereqs.
func (r *CalicoManager) uploadDir() string {
	return r.outputDir
}

// TODO: We should produce a tar per architecture that we ship.
// TODO: We should produce windows tars
func (r *CalicoManager) buildReleaseTar() error {
	a := r.archive()
	sources, err := r.archiveSources()
	if err != nil {
		return err
	}
	a.Sources = sources
	return archives.Build(
		a,
		archives.WithRunner(r.runner),
		archives.WithLogsDir(r.logsDir),
	)
}

func (r *CalicoManager) archiveSources() ([]archives.Contributor, error) {
	var sources []archives.Contributor
	if r.archiveImages {
		imgs, err := images.Archive(r.repoRoot, r.calicoVersion.String(), r.imageReleaseDirs,
			images.WithRunner(r.runner),
			images.WithRegistries(r.imageRegistries...),
			images.WithArches(r.architectures...),
			images.WithPull(r.isHashRelease && !r.images))
		if err != nil {
			return nil, fmt.Errorf("archive images: %w", err)
		}
		sources = append(sources, imgs...)
	}
	if r.binaries {
		sources = append(sources, binaries.Archive(r.repoRoot)...)
	}
	if r.manifests {
		sources = append(sources, manifests.Archive(r.manifestsRoot()))
	}
	return sources, nil
}

func (r *CalicoManager) manifestsRoot() string {
	if r.isHashRelease {
		return r.hashrelease.Source
	}
	return r.repoRoot
}

func (r *CalicoManager) binariesBuilt() error {
	var errs []error
	if r.binaries {
		errs = append(errs, binaries.Built(r.repoRoot, r.architectures))
	}
	if r.e2eBinaries && r.isHashRelease {
		errs = append(errs, binaries.E2EBuilt(r.architectures, r.uploadDir()))
	}
	return errors.Join(errs...)
}

func (r *CalicoManager) buildBinaries() error {
	var builders []binaries.Builder
	if r.binaries {
		builders = append(builders, binaries.Release(r.uploadDir(), r.architectures...)...)
	}
	if r.e2eBinaries && r.isHashRelease {
		builders = append(builders, binaries.E2E(r.architectures, binaries.E2EDir(r.uploadDir())))
	}
	if len(builders) == 0 {
		logrus.Info("Skip building binaries")
		return nil
	}
	if err := binaries.Build(
		r.repoRoot, r.calicoVersion.String(),
		builders,
		binaries.WithRunner(r.runner),
		binaries.WithLogsDir(r.logsDir),
	); err != nil {
		return fmt.Errorf("build binaries: %w", err)
	}
	if r.e2eBinaries && r.isHashRelease {
		if err := binaries.PackageE2EConfig(r.repoRoot, r.uploadDir()); err != nil {
			return fmt.Errorf("package e2e config: %w", err)
		}
	}
	return nil
}

func (r *CalicoManager) buildContainerImages() error {
	err := images.Build(
		r.repoRoot, r.calicoVersion.String(),
		images.NarrowVariants(images.BuildVariants, r.imageReleaseDirs),
		images.WithRunner(r.runner),
		images.WithRegistries(r.imageRegistries...),
		images.WithArches(r.architectures...),
		images.WithLogsDir(r.logsDir),
		images.WithPruneCache(r.ci),
	)
	if err != nil {
		return fmt.Errorf("build images: %w", err)
	}
	return nil
}

func (r *CalicoManager) publishContainerImages() error {
	opts := []images.PublishOption{
		images.WithRunner(r.runner),
		images.WithRegistries(r.imageRegistries...),
		images.WithArches(r.architectures...),
		images.WithLogsDir(r.logsDir),
		images.WithDependencies(r.dependencies()...),
	}
	if scan := r.scanRequest(); scan != nil {
		opts = append(opts, images.WithScan(scan))
	}
	if r.retagImages {
		opts = append(opts, images.WithRetag(r.fromRegistry, r.fromTag, !r.gitRef))
	}
	if err := images.Publish(
		r.repoRoot, r.calicoVersion.String(),
		images.NarrowVariants(images.PublishVariants, r.imageReleaseDirs),
		r.recordsDir, !r.planOptions.Local, r.digestResolver(), opts...,
	); err != nil {
		return fmt.Errorf("publish images: %w", err)
	}
	return r.publishBranchTag()
}

func (r *CalicoManager) imagesAction() func() error {
	if r.images {
		return r.publishContainerImages
	}
	return r.resolveContainerImages
}

func (r *CalicoManager) dependencies() []images.Dependency {
	return append([]images.Dependency{r.resolveOperator}, r.imageDependencies...)
}

// A hashrelease hands out the pinned operator even when this run did not
// publish it, as an earlier run may have.
func (r *CalicoManager) resolveOperator() ([]string, error) {
	if !r.isHashRelease || r.includesOperator() {
		return nil, nil
	}
	err := operator.Resolve(
		r.operatorConfig(),
		operator.Variants(),
		r.recordsDir,
		operator.WithRunner(r.runner),
		operator.WithArches(r.architectures...),
		operator.WithLogsDir(r.logsDir),
		operator.WithResolver(r.digestResolver()),
	)
	return r.checkResolved(r.operatorImage, false, err)
}

func (r *CalicoManager) includesOperator() bool {
	return r.operator.Do != nil && !r.planOptions.Skip[operator.Step]
}

// checkResolved lets an excluded component's missing images pass, and leaves
// the missing ones out of the scan.
func (r *CalicoManager) checkResolved(component string, included bool, err error) ([]string, error) {
	missing, onlyMissing := images.OnlyMissing(err)
	if !onlyMissing || included {
		return nil, err
	}
	logrus.WithField("images", missing).Warn("Images this run does not publish are missing")
	if ref := r.componentImages()[component]; slices.Contains(missing, ref) {
		return []string{ref}, nil
	}
	return nil, nil
}

func (r *CalicoManager) resolveContainerImages() error {
	opts := []images.ResolveOption{
		images.WithRunner(r.runner),
		images.WithRegistries(r.imageRegistries...),
		images.WithArches(r.architectures...),
		images.WithLogsDir(r.logsDir),
		images.WithDependencies(r.dependencies()...),
	}
	if scan := r.scanRequest(); scan != nil {
		opts = append(opts, images.WithScan(scan))
	}
	err := images.Resolve(
		r.repoRoot, r.calicoVersion.String(),
		images.NarrowVariants(images.PublishVariants, r.imageReleaseDirs),
		r.recordsDir, r.digestResolver(),
		opts...,
	)
	// A dry run pushes nothing, so images it built are never in the registry.
	if missing, onlyMissing := images.OnlyMissing(err); onlyMissing && r.planOptions.Local {
		logrus.WithField("images", missing).Warn("Dry run, not resolving unpublished images")
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve images: %w", err)
	}
	return nil
}

// digestResolver reports a published tag's digest, defaulting to the registry.
// Tests substitute one so they never reach the network.
func (r *CalicoManager) digestResolver() registry.DigestResolver {
	if r.resolveDigest != nil {
		return r.resolveDigest
	}
	return registry.ResolveDigest
}

// The images come from the pinned components, so a hashrelease scans what it
// pinned.
func (r *CalicoManager) scanRequest() *images.ScanRequest {
	if !r.imageScanning {
		return nil
	}
	return &images.ScanRequest{
		Config:      r.imageScanningConfig,
		ProductCode: r.productCode,
		Images:      slices.Collect(maps.Values(r.componentImages())),
		Stream:      r.calicoVersion.PrimaryStream(),
		Release:     !r.isHashRelease,
		OutputDir:   r.tmpDir,
		DryRun:      r.planOptions.Local,
	}
}

// publishBranchTag moves the branch-named tag (e.g. release-v3.33) onto the
// images just published, so the branch always has a pullable tag between
// official releases.
func (r *CalicoManager) publishBranchTag() error {
	reg := r.imageRegistries[0]
	branch, err := r.releaseBranch()
	if err != nil {
		return fmt.Errorf("release branch: %w", err)
	}
	if branch == "" {
		return nil
	}
	if registry.DefaultProductRegistry != reg {
		logrus.WithFields(logrus.Fields{
			"registry": reg,
			"branch":   branch,
		}).Warning("Skip moving the branch tag outside the default registry")
		return r.publishOperatorBranchTag(branch)
	}
	registry, err := r.getRegistryFromManifests()
	if err != nil {
		return fmt.Errorf("get registry from manifests: %w", err)
	}
	variants := []images.Variant{{
		Name:        images.StandardVariant,
		Target:      branchTagTarget,
		ReleaseDirs: images.VariantDirs(images.StandardVariants(images.PublishVariants)),
	}}
	if len(windowsBranchTagDirs) > 0 {
		variants = append(variants, images.Variant{
			Name:        images.WindowsVariant,
			Target:      windowsBranchTagTarget,
			ReleaseDirs: slices.Clone(windowsBranchTagDirs),
		})
	}
	// The arch images must carry the branch tag before the manifest can list
	// them as its children.
	if err := images.Publish(
		r.repoRoot, branch,
		images.NarrowVariants(variants, r.imageReleaseDirs),
		"", !r.planOptions.Local, r.digestResolver(),
		images.WithRunner(r.runner),
		images.WithRegistries(registry),
		images.WithArches(r.architectures...),
		images.WithLogsDir(r.logsDir),
		images.WithStepName("images-publish-branch"),
		images.WithRetag(reg, r.calicoVersion.String(), true),
	); err != nil {
		return fmt.Errorf("publish branch %s tag images: %w", branch, err)
	}
	return r.publishOperatorBranchTag(branch)
}

// releaseBranch returns "" for a branch with no branch tag. An official release
// runs on its prep branch, so it takes the release branch prep recorded.
func (r *CalicoManager) releaseBranch() (string, error) {
	if r.releaseBranchPrefix == "" {
		return "", fmt.Errorf("release branch prefix is not set")
	}
	branch, err := r.determineBranch()
	if err != nil {
		return "", err
	}
	if branch == defaultBranch {
		return branch, nil
	}
	if branch == r.prepBranchName() {
		out, err := r.git("log", "-1", "-i", "--grep=^"+gitTrailerReleaseBranch+": ",
			"--format=%(trailers:key="+gitTrailerReleaseBranch+",valueonly)")
		if err != nil {
			return "", fmt.Errorf("read %s trailer: %w", gitTrailerReleaseBranch, err)
		}
		if branch, _, _ = strings.Cut(strings.TrimSpace(out), "\n"); branch == "" {
			return "", fmt.Errorf("no %s trailer on %s", gitTrailerReleaseBranch, r.prepBranchName())
		}
	}
	rb, ok := version.ParseReleaseBranch(r.releaseBranchPrefix, branch)
	if !ok {
		logrus.WithField("branch", branch).Info("Not on a release branch, skipping branch tag publish")
		return "", nil
	}
	return rb.String(), nil
}

func (r *CalicoManager) publishOperatorBranchTag(branch string) error {
	if !r.includesOperator() {
		logrus.WithField("branch", branch).Info("Skip moving the operator branch tag: the operator is off")
		return nil
	}
	if registry.DefaultOperatorRegistry != r.operatorRegistry {
		logrus.WithFields(logrus.Fields{
			"registry": r.operatorRegistry,
			"branch":   branch,
		}).Info("Skip moving the operator branch tag outside the default registry")
		return nil
	}

	// The operator publishes to its own registries, so it takes a pass of its own.
	if err := operator.PublishBranchTag(
		r.operatorConfig(),
		operator.Variants(), branch,
		operator.WithRunner(r.runner),
		operator.WithArches(r.architectures...),
		operator.WithLogsDir(r.logsDir),
		operator.WithDryRun(r.planOptions.Local),
	); err != nil {
		return fmt.Errorf("publish branch %s tag operator image: %w", branch, err)
	}
	return nil
}

func (r *CalicoManager) operatorConfig() operator.Operator {
	return operator.Operator{
		RepoRoot:   r.repoRoot,
		Version:    r.operatorVersion,
		Image:      r.operatorImage,
		Registries: []string{r.operatorRegistry},
	}
}

func (r *CalicoManager) publishHelmCharts() error {
	return charts.Publish(
		r.chart(), r.helmRegistries, r.recordsDir, !r.planOptions.Local,
		charts.WithRunner(r.runner),
		charts.WithLogsDir(r.logsDir),
		charts.WithResolver(r.digestResolver()),
	)
}

// buildReleaseNotes writes a release's notes into the repo, for the prep commit.
// A hashrelease previews the next release, so its notes are for that version
// and go to the output dir.
func (r *CalicoManager) buildReleaseNotes(outDir string, ver version.Version) error {
	_, err := outputs.ReleaseNotes(github.Client(r.runner), r.repo.Owner, r.repoRoot, outDir, ver)
	return err
}

func (r *CalicoManager) assertReleaseNotesPresent(ver string) error {
	releaseNotesPath := outputs.ReleaseNoteFilePath(r.repoRoot, ver)
	releaseNotesStat, err := os.Stat(releaseNotesPath)
	if err != nil {
		return fmt.Errorf("release notes file is invalid: %s", err.Error())
	}

	if releaseNotesStat.Size() == 0 {
		return fmt.Errorf("release notes file is invalid: file is 0 bytes")
	} else if releaseNotesStat.IsDir() {
		return fmt.Errorf("release notes file is invalid: %s is a directory", releaseNotesPath)
	}
	return nil
}

// determineBranch returns the current checked out branch.
func (r *CalicoManager) determineBranch() (string, error) {
	out, err := r.git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		logrus.WithError(err).Error("Error determining branch")
		return "", fmt.Errorf("error determining branch: %w", err)
	} else if strings.TrimSpace(out) == "HEAD" {
		logrus.Error("Not on a branch, refusing to cut release")
		return "", fmt.Errorf("not on a branch")
	}
	return strings.TrimSpace(out), nil
}

func (r *CalicoManager) git(args ...string) (string, error) {
	return command.Git(r.runner).In(r.repoRoot).Run(args...)
}

func (r *CalicoManager) runPlan(nodes []plan.Node) error {
	p, err := plan.New(r.planOptions, nodes...)
	if err != nil {
		return err
	}
	return p.Run()
}
