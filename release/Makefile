include ../metadata.mk

PACKAGE_NAME = github.com/projectcalico/calico/release

include ../lib.Makefile

export ORGANIZATION
export GIT_REPO
export RELEASE_BRANCH_PREFIX
export DEV_TAG_SUFFIX
export DEV_REGISTRIES
export OPERATOR_ORGANIZATION
export OPERATOR_GIT_REPO
export OPERATOR_BRANCH

.PHONY: build
build: bin/release

clean:
	@rm -rf ./bin ./output ./_output ./report ./tmp
	@find ../ -name '*release*.log' -delete
	@find . -name '*.received.*' -delete

bin/release: $(shell find . -name "*.go")
	@mkdir -p bin && \
	$(call build_binary, ./cmd, bin/release)

POSTRELEASE_TEST_PACKAGE		:= $(PACKAGE_NAME)/pkg/postrelease
UNIT_TEST_PACKAGES	:= $(shell go list ./... | grep -v $(POSTRELEASE_TEST_PACKAGE))
ut:
	@mkdir -p report
	@$(DOCKER_GO_BUILD) gotestsum --junitfile report/unit-tests.xml \
		-- -coverprofile=report/coverage.out $(UNIT_TEST_PACKAGES)

###############################################################################
# CI/CD
###############################################################################
.PHONY: ci
ci: static-checks ut

###############################################################################
# Hashrelease
###############################################################################
.PHONY: hashrelease hashrelease-build hashrelease-publish
hashrelease: hashrelease-build hashrelease-publish

# Ensures helm is in the bin dir relative to repo root
../bin/helm:
	@$(MAKE) -C ../ helm

hashrelease-build: bin/release var-require-all-GITHUB_TOKEN
	@bin/release hashrelease build

hashrelease-publish: bin/release var-require-all-GITHUB_TOKEN ../bin/helm
	@bin/release hashrelease publish

hashrelease-svr-gc: bin/release
	@bin/release hashrelease garbage-collect

###############################################################################
# Release
###############################################################################
.PHONY: release-notes
release-notes: bin/release var-require-all-GITHUB_TOKEN
	@bin/release release generate-release-notes

GH_TEAM = release-team
check-release-prereq: var-require-all-CI-GITHUB_TOKEN-CI_USER
ifneq ($(CI), true)
	$(error This target should only be run in CI)
endif
# Check if the user is an active member of the release team in the organization
ifneq ($(shell gh api --method GET -H "Accept: application/vnd.github.v3+json" \
	/orgs/$(ORGANIZATION)/teams/${GH_TEAM}/memberships/${CI_USER} --jq '.state' 2>/dev/null), active)
	$(error User '$(CI_USER)' is not an active member of the '$(GH_TEAM)' team in the '$(ORGANIZATION)' organization)
endif