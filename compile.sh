#!/bin/sh
set -e
set -x

AWE="."

GIT_AWE_VERSION=$(cd ${AWE} ; git describe)

# use this for single binary
CGO_ENABLED=0 go install -a -installsuffix cgo -v -ldflags="-X github.com/MG-RAST/AWE/lib/conf.VERSION=${GIT_AWE_VERSION}" ./awe-server/

# use this for race debug flag
#go install -a -v -race -ldflags "-X github.com/MG-RAST/AWE/lib/conf.GIT_COMMIT_HASH=${GITHASH}" ./awe-worker/ ./awe-server/

echo "recompile with: CGO_ENABLED=0 go install ./awe-server"