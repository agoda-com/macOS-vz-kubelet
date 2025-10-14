ROOT_DIR := $(shell dirname $(realpath $(lastword $(MAKEFILE_LIST))))

RELEASE_CERTIFICATE_NAME ?= ""
RELEASE_PROVISION_PROFILE_PATH ?= ""

include makefiles/go.mk

e2e-test: GOTESTARGS += -timeout 15m \
	--namespace e2e \
	--node-name $(NODE_NAME) \
	$(if $(E2E_MACOS_IMAGE),--macos-image $(E2E_MACOS_IMAGE)) \
	$(if $(BUSYBOX_IMAGE),--busybox-image $(BUSYBOX_IMAGE)) \
	$(if $(DOCKER_SOCKET_PATH),--docker-socket-path $(DOCKER_SOCKET_PATH)) \
	$(if $(E2E_MACOS_IMAGE_DIR),--macos-image-dir "$(E2E_MACOS_IMAGE_DIR)") \
	-exec $(realpath $(dir $(firstword $(MAKEFILE_LIST)))/makefiles/scripts/sign-and-run.sh)

.PHONY: snapshot release

snapshot:
	ROOT_DIR=$(ROOT_DIR) \
	RELEASE_CERTIFICATE_NAME=$(RELEASE_CERTIFICATE_NAME) \
	RELEASE_PROVISION_PROFILE_PATH=$(RELEASE_PROVISION_PROFILE_PATH) \
		goreleaser release --clean --snapshot

release:
	ROOT_DIR=$(ROOT_DIR) \
	RELEASE_CERTIFICATE_NAME=$(RELEASE_CERTIFICATE_NAME) \
	RELEASE_PROVISION_PROFILE_PATH=$(RELEASE_PROVISION_PROFILE_PATH) \
		goreleaser release --clean
