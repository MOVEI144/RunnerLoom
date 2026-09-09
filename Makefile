GO ?= go
PYTHON ?= python3.12
.PHONY: build test check package clean
build:
	mkdir -p dist
	CGO_ENABLED=0 $(GO) build -trimpath -o dist/runnerloom ./cmd/runnerloom
test:
	$(GO) test -race -count=1 ./...
check:
	test -z "$$(gofmt -l cmd internal)"
	$(GO) vet ./...
	$(GO) test -race -count=1 ./...
	for s in internal/cli/image-build.sh scripts/ci-vm-smoke.sh scripts/ci-free-space.sh; do bash -n "$$s" || exit 1; done
package:
	$(GO) mod vendor
	GO=$(GO) $(PYTHON) scripts/package.py --deb
	$(PYTHON) scripts/test_package.py dist
clean:
	rm -rf -- dist
