GIT_COMMIT := $(shell git rev-list -1 HEAD)
BUILD_TIME := $(shell date -u +%Y%m%d.%H%M)
CWD := $(shell dirname $(realpath $(firstword $(MAKEFILE_LIST))))

# Build binary to ./
default: check
	go build -ldflags '-X main.versionGitCommit=${GIT_COMMIT} -X main.versionBuildTime=${BUILD_TIME}' -gcflags=all="-N -l" ./cmd/acceld
	go build -ldflags '-X main.versionGitCommit=${GIT_COMMIT} -X main.versionBuildTime=${BUILD_TIME}' -gcflags=all="-N -l" ./cmd/accelctl

install-check-tools:
	@curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(shell go env GOPATH)/bin v1.42.1

check:
	@echo "$@"
	@$(shell go env GOPATH)/bin/golangci-lint run
	@$(shell go env GOPATH)/bin/golangci-lint run

# Run unit testing
# Run a particular test in this way:
# go test -v -count=1 -run TestFoo ./pkg/...
ut: default
	go test -count=1 -v ./pkg/...

# Run integration testing
smoke: default
	go test -count=1 -v ./test

# Run testing 
test: default ut smoke

release-image:
	docker build -t goharbor/harbor-acceld -f script/release/Dockerfile .
	docker run -v $(CWD)/misc/config/config.yaml.nydus.tmpl:/etc/acceld-config.yaml -it -d --rm -p 2077:2077 goharbor/harbor-acceld /etc/acceld-config.yaml
	sleep 5
	curl -f http://127.0.0.1:2077/api/v1/health
