package postrelease

import (
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cast"
	"go.yaml.in/yaml/v3"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"

	"github.com/projectcalico/calico/release/internal/command"
	"github.com/projectcalico/calico/release/internal/registry"
	"github.com/projectcalico/calico/release/internal/utils"
)

func chartURLs(githubOrg, githubRepo, version string) []string {
	urls := []string{}
	for _, chart := range utils.AllReleaseCharts() {
		u := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s-%s.tgz", githubOrg, githubRepo, version, chart, version)
		urls = append(urls, u)
	}
	return urls
}

// ociVersion is the strict-semver chart version (no "v" prefix) used as the
// OCI tag. The GitHub-release filename and index URL keep the "v" prefix; the
// OCI tag and the OCI-pulled filename do not. See issue #12826.
func ociVersion() string {
	return strings.TrimPrefix(releaseVersion, "v")
}

func TestHelmChart(t *testing.T) {
	t.Parallel()

	checkVersion(t, releaseVersion)

	t.Run("github", func(t *testing.T) {
		t.Parallel()

		for _, url := range chartURLs(githubOrg, githubRepo, releaseVersion) {
			resp, err := http.Get(url)
			if err != nil {
				t.Fatalf("failed to fetch helm chart: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("failed to fetch helm chart: server returned %s", resp.Status)
			}
			defer func() { _ = resp.Body.Close() }()

			chart, err := loader.LoadArchive(resp.Body)
			if err != nil {
				t.Fatalf("load helm chart: %v", err)
			}
			validateChart(t, chart)
		}
	})

	t.Run("OCI registry", func(t *testing.T) {
		t.Parallel()

		for _, reg := range registry.DefaultHelmRegistries {
			t.Run(reg, func(t *testing.T) {
				t.Parallel()

				// Regression for #12826: with no --version, Helm must resolve
				// the latest tag. This only works if the OCI tag is strict
				// semver (no "v" prefix).
				noVerDir := t.TempDir()
				noVerArgs := []string{
					"pull", fmt.Sprintf("oci://%s/%s", reg, utils.TigeraOperatorChart),
				}
				if out, err := command.RunInDir(noVerDir, "helm", noVerArgs); err != nil {
					t.Fatalf("pull %s helm chart (no --version) from %s: %v\nOutput: %s", utils.TigeraOperatorChart, reg, err, out)
				}

				// Explicit --version uses the strict-semver tag, and the pulled
				// file is named by the chart's internal (stripped) version.
				dir := t.TempDir()
				args := []string{
					"pull", fmt.Sprintf("oci://%s/%s", reg, utils.TigeraOperatorChart),
					"--version", ociVersion(),
				}
				out, err := command.RunInDir(dir, "helm", args)
				if err != nil {
					t.Fatalf("pull %s %s helm chart from %s: %v\nOutput: %s", utils.TigeraOperatorChart, ociVersion(), reg, err, out)
				}
				chart, err := loader.Load(filepath.Join(dir, fmt.Sprintf("%s-%s.tgz", utils.TigeraOperatorChart, ociVersion())))
				if err != nil {
					t.Fatalf("load helm chart from %s: %v", reg, err)
				}
				validateChart(t, chart)
			})
		}
	})
}

func validateChart(t testing.TB, chart *chart.Chart) {
	t.Helper()
	if err := chart.Validate(); err != nil {
		t.Fatalf("invalid helm chart: %v", err)
	}
	if chart.AppVersion() != releaseVersion {
		t.Fatalf("expected helm chart app version %s, got %s", releaseVersion, chart.AppVersion())
	}
}

type helmIndex struct {
	Entries map[string][]map[string]any `yaml:"entries"`
}

func TestHelmIndex(t *testing.T) {
	t.Parallel()

	checkVersion(t, releaseVersion)

	indexURL, err := url.JoinPath(utils.CalicoHelmRepoURL, "index.yaml")
	if err != nil {
		t.Fatalf("construct helm index url: %v", err)
	}
	resp, err := http.Get(indexURL)
	if err != nil {
		t.Fatalf("failed to fetch helm index: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("failed to fetch helm index: server returned %s", resp.Status)
	}
	defer func() { _ = resp.Body.Close() }()
	index := helmIndex{}
	if err := yaml.NewDecoder(resp.Body).Decode(&index); err != nil {
		t.Fatalf("failed to decode helm index: %v", err)
	}
	if len(index.Entries) == 0 {
		t.Fatalf("helm index is empty")
	}
	tigeraOperatorEntries, ok := index.Entries["tigera-operator"]
	if !ok || len(tigeraOperatorEntries) == 0 {
		t.Fatalf("helm index does not contain tigera-operator entries")
	}
	filteredEntries := slices.Collect(func(yield func(map[string]any) bool) {
		for _, entry := range tigeraOperatorEntries {
			if entry["version"].(string) == ociVersion() {
				yield(entry)
			}
		}
	})
	if len(filteredEntries) == 0 {
		t.Fatalf("helm index does not contain tigera-operator entry for version %s", ociVersion())
	} else if len(filteredEntries) > 1 {
		t.Fatalf("helm index contains multiple tigera-operator entries for version %s", ociVersion())
	}
	helmEntry := filteredEntries[0]
	urls, ok := helmEntry["urls"]
	if !ok || len(urls.([]any)) == 0 {
		t.Fatalf("helm index entry for version %s does not contain urls", ociVersion())
	}

	for _, url := range chartURLs(githubOrg, githubRepo, releaseVersion) {
		if !slices.Contains(cast.ToStringSlice(urls), url) {
			t.Fatalf("helm index entry for version %s does not contain expected URL: %s", releaseVersion, url)
		}
	}
}
