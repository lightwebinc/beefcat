# GOWORK=off everywhere on purpose: a workspace would resolve bcommon to a
# local checkout, so a go.mod skew would stay invisible here. What is tested
# is what go.mod pins.

.PHONY: verify test vet fmt-check deps-check licences licences-update beefcat devchain quickstart-check

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

verify: fmt-check vet deps-check licences test quickstart-check

test:
	GOWORK=off go test -race -count=1 ./...

vet:
	GOWORK=off go vet ./...

# The command, and the local chain, into bin/.
beefcat:
	GOWORK=off CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/beefcat ./cmd/beefcat

devchain:
	GOWORK=off CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$(VERSION)" -o bin/devchain ./cmd/devchain

fmt-check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt:"; gofmt -l .; exit 1; }

# The direct dependencies, asserted rather than intended:
#  - go.mod and go.sum are tidy, and no module is replaced;
#  - the direct set is exactly go-sdk, bcommon and bcommon's devkit
#    (imported only by the devchain command);
#  - go-sdk resolves to exactly the version bcommon pins. Minimal version
#    selection takes the higher of two pins, so a library asking for a
#    later go-sdk would change what beefcat ships without a line changing
#    here.
DEPS_DIRECT := github.com/bsv-blockchain/go-sdk github.com/lightwebinc/bcommon github.com/lightwebinc/bcommon/devkit
DEPS_SDK    := github.com/bsv-blockchain/go-sdk v1.7.8

deps-check:
	@set -eu; \
	if ! GOWORK=off go mod tidy -diff >/dev/null; then \
		echo "deps-check: go.mod or go.sum is not tidy; run go mod tidy"; exit 1; \
	fi; \
	replaced=$$(GOWORK=off go list -m -f '{{if .Replace}}{{.Path}}{{end}}' all); \
	if [ -n "$$replaced" ]; then echo "deps-check: no module may be replaced, found: $$replaced"; exit 1; fi; \
	direct=$$(GOWORK=off go list -m -f '{{if not (or .Main .Indirect)}}{{.Path}}{{end}}' all | sed '/^$$/d' | sort); \
	want=$$(printf '%s\n' $(DEPS_DIRECT) | sort); \
	if [ "$$direct" != "$$want" ]; then \
		echo "deps-check: the direct dependencies must be exactly:"; echo "$$want" | sed 's/^/  /'; \
		echo "found:"; echo "$$direct" | sed 's/^/  /'; exit 1; \
	fi; \
	sdk=$$(GOWORK=off go list -m $(firstword $(DEPS_SDK))); \
	if [ "$$sdk" != "$(DEPS_SDK)" ]; then echo "deps-check: go-sdk must resolve to exactly $(lastword $(DEPS_SDK)), found: $$sdk"; exit 1; fi

# LICENSE-THIRD-PARTY is generated from what the binaries link, so a new
# dependency brings its license obligation into the file rather than
# leaving it undischarged. licences fails when the file is stale.
licences:
	python3 scripts/gen-third-party-licenses.py . --check

licences-update:
	python3 scripts/gen-third-party-licenses.py .

# QUICKSTART.md's sandbox, run as written: a local chain, a funded sender,
# a stream sent and verified, and each tampered stream refused.
quickstart-check: beefcat devchain
	sh scripts/quickstart-check.sh
