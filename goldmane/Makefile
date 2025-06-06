include ../metadata.mk

PACKAGE_NAME = github.com/projectcalico/calico/goldmane
IMAGE_BUILD_MARKER = goldmane_container-$(ARCH).created

###############################################################################
# include ../lib.Makefile
#   Additions to EXTRA_DOCKER_ARGS need to happen before the include since
#   that variable is evaluated when we declare DOCKER_RUN and siblings.
###############################################################################
include ../lib.Makefile

# Configure variables used by ci/cd common targets from lib.Makefile.
BUILD_IMAGES=goldmane

# Full set of binaries to build.
BINARIES=bin/goldmane-$(ARCH) bin/health-$(ARCH)

.PHONY: image build
image: $(IMAGE_BUILD_MARKER)
build: $(BINARIES)
clean:
	rm -rf bin
	rm -f $(IMAGE_BUILD_MARKER)

image-all: $(addprefix sub-image-,$(VALIDARCHES))
sub-image-%:
	$(MAKE) image ARCH=$*

# Build goldmane image.
goldmane calico/goldmane: $(IMAGE_BUILD_MARKER)
$(IMAGE_BUILD_MARKER): $(BINARIES)
	$(DOCKER_BUILD) --build-arg TARGETARCH=$(ARCH) -t goldmane:latest-$(ARCH) -f docker/Dockerfile .
	$(MAKE) retag-build-images-with-registries BUILD_IMAGES=$(BUILD_IMAGES) VALIDARCHES=$(ARCH) IMAGETAG=latest
	touch $@

bin/goldmane-$(ARCH): $(shell find . -name '*.go')
	$(call build_binary, $(PACKAGE_NAME)/cmd/, $@)

bin/health-$(ARCH): $(shell find . -name '*.go')
	$(call build_binary, $(PACKAGE_NAME)/cmd/health, $@)

# Build the testserver image.
.PHONY: testserver
calico/flowgen: bin/flowgen
	docker build -t calico/flowgen -f docker/flowgen/Dockerfile .

bin/flowgen: $(shell find . -name '*.go')
	$(call build_binary, $(PACKAGE_NAME)/cmd/flowgen, $@)

# Update protobuf generation.
protobuf proto/api.pb.go: proto/api.proto
	$(DOCKER_RUN) -v $(CURDIR):/code $(CALICO_BUILD) sh -c 'protoc --proto_path=/code/proto --go_out=/code/proto --go-grpc_out=. --go_opt=paths=source_relative /code/proto/api.proto'
	$(MAKE) fix-changed

###############################################################################
# Generate files
###############################################################################
GENERATED_FILES=proto/api.pb.go
gen-files: $(GENERATED_FILES) gen-mocks

###############################################################################
# UTs
###############################################################################
ci: static-checks ut
ut:
	$(DOCKER_GO_BUILD) go test ./... -coverprofile coverage.profile -race -count 1

gen-mocks:
	$(DOCKER_RUN) $(CALICO_BUILD) sh -c 'mockery'

###############################################################################
# Release
###############################################################################
## Deploys images to registry
cd: image-all cd-common

release-build: .release-$(VERSION).created
.release-$(VERSION).created:
	$(MAKE) clean image-all RELEASE=true
	$(MAKE) retag-build-images-with-registries RELEASE=true IMAGETAG=$(VERSION)
	$(MAKE) retag-build-images-with-registries RELEASE=true IMAGETAG=latest
	touch $@

release-publish: release-prereqs .release-$(VERSION).published
.release-$(VERSION).published:
	$(MAKE) push-images-to-registries push-manifests IMAGETAG=$(VERSION) RELEASE=$(RELEASE) CONFIRM=$(CONFIRM)
	touch $@
