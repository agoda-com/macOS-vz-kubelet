ROOT_DIR := $(shell dirname $(realpath $(lastword $(MAKEFILE_LIST))))

RELEASE_CERTIFICATE_NAME ?= ""
RELEASE_PROVISION_PROFILE_PATH ?= ""

include makefiles/go.mk

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
