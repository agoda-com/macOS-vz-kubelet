#!/usr/bin/env bash
# sign-and-run.sh

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

binary="$1"
shift

# 1) codesign the test binary in place
codesign --entitlements "$SCRIPT_DIR/../../resources/vz.entitlements" -s - "$binary" \
    || exit 1

# 2) exec into it with the original args
# Use sudo to allow access to local network for SSH exec test
exec sudo --preserve-env=VZ_SSH_USER \
    --preserve-env=VZ_SSH_PASSWORD \
    --preserve-env=APISERVER_CA_CERT_LOCATION \
    --preserve-env=APISERVER_CERT_LOCATION \
    --preserve-env=APISERVER_KEY_LOCATION \
    --preserve-env=NODE_NAME \
    -- "$binary" "$@"
