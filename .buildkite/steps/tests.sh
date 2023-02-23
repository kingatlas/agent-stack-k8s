#!/bin/bash
set -euxo pipefail

go version
go install gotest.tools/gotestsum

echo '+++ Running tests'
export IMAGE=$(buildkite-agent meta-data get "agent-image")

gotestsum --junitfile "junit-${BUILDKITE_JOB_ID}.xml" -- -count=1 -failfast "$@" ./...