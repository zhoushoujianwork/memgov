.PHONY: init tidy build install web-build web-check web-dev test test-runtime test-runtime-race test-scenarios test-scenario1-acceptance test-app-channel build-scenario-driver vet fmt fmt-check check release clean

export GOFLAGS = -mod=readonly
BIN = .memgov/bin/memgov
VERSION ?= 2.0.0-rc1

init: install
	$(BIN) init

tidy:
	go mod tidy

web/node_modules/.package-lock.json: web/package.json web/package-lock.json
	npm ci --prefix web

web-build: web/node_modules/.package-lock.json
	npm run build --prefix web

web-check: web/node_modules/.package-lock.json
	npm run check --prefix web

web-dev: web/node_modules/.package-lock.json
	npm run dev --prefix web

build: web-build
	go build ./...
	go build -trimpath -ldflags '-X github.com/zhoushoujianwork/memgov/internal/cli.Version=$(VERSION)' -o $(BIN).new ./cmd/memgov
	mv -f $(BIN).new $(BIN)

install: build

test:
	go test ./...

test-runtime:
	go test ./internal/agent ./internal/channel/dws ./internal/core ./internal/runlog ./internal/runtime -count=1

test-runtime-race:
	go test -race ./internal/channel ./internal/channel/dws ./internal/core ./internal/runlog ./internal/runtime

# Framework replay only; no actual DingTalk access.
test-scenarios:
	go test ./tests/scenarios -run 'Test(Framework|ScenarioFixtures)' -count=1 -v

# Real scenario-driver binary against offline fake dws, not live business acceptance.
test-scenario1-acceptance:
	go test ./tests/scenarios -run '^TestScenario1Acceptance$$' -count=1 -v

# Application-bot intake against simulated frames. This exercises the parser,
# credential stripping and acknowledgement rules; it is not a real DingTalk
# integration pass, which needs application credentials and a live Stream.
test-app-channel:
	go test ./internal/channel/dingtalkapp -count=1 -v
	go test ./internal/cli -run 'TestApp|TestAudienceStaysSeparate' -count=1 -v

build-scenario-driver:
	go build -o .memgov/bin/scenario-driver ./cmd/scenario-driver

vet:
	go vet ./...

fmt:
	gofmt -l -w cmd internal tests

fmt-check:
	@out=$$(gofmt -l cmd internal tests); if [ -n "$$out" ]; then echo "$$out"; exit 1; fi

check: web-check fmt-check vet test

release: check
	bash scripts/release.sh '$(VERSION)'

# Only generated binaries and release archives; never remove memory data.
clean:
	rm -rf .memgov/bin dist
