git_commit = $(shell git log -1 --pretty=format:"%H")

test_unit:
	go clean --cache
	go test -v -race $(shell go list ./... | grep -v /functional_tests)
	go test -v ./pkg/shipyard

test_functional: install_local
	cd ./functional_tests && go test -timeout 20m -v -run.test true ./...

test_docker:
	docker build -t shipyard-run/tests -f Dockerfile.test .
	docker run --rm shipyard-run/tests bash -c 'go test -v -race -coverprofile=coverage.txt -covermode=atomic $(go list ./... | grep -v /functional_tests)'
	docker run --rm shipyard-run/tests bash -c 'go test -v ./pkg/shipyard'

# Test Github actions using `act`
# https://github.com/nektos/act
test_github_actions:
	act -P ubuntu-latest=nektos/act-environments-ubuntu:18.04 -j build
	act -P ubuntu-latest=nektos/act-environments-ubuntu:18.04 -j functional_test

test: test_unit test_functional

# Run tests continually with  a watcher
autotest:
	filewatcher --idle-timeout 24h -x **/functional_tests gotestsum --format standard-verbose

build: build-darwin build-linux build-windows

build-darwin:
	CGO_ENABLED=0 GOOS=darwin go build -ldflags "-X main.version=${git_commit}" -o bin/yard-darwin main.go

build-linux:
	CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.version=${git_commit}" -o bin/yard-linux main.go

build-windows:
	CGO_ENABLED=0 GOOS=windows go build -ldflags "-X main.version=${git_commit}" -o bin/yard-windows.exe main.go

install_local:
	go build -ldflags "-X main.version=${git_commit}" -o bin/yard-dev main.go
	sudo cp bin/yard-dev /usr/local/bin/yard-dev
