GO ?= go
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
	bash -n internal/cli/image-build.sh scripts/ci-vm-smoke.sh scripts/ci-free-space.sh
package:
	$(GO) mod vendor
	GO=$(GO) python3 scripts/package.py --deb
	python3 scripts/test_package.py dist
clean:
	rm -rf -- dist
