MODULES := $(patsubst %/go.mod,%,$(shell find . -name go.mod -not -path './vendor/*' -not -path './.git/*'))

.PHONY: test build lint fmt

test:
	@for d in $(MODULES); do \
		echo "== test $$d =="; \
		(cd $$d && go test ./... -count=1 -timeout 120s) || exit 1; \
	done

build:
	@for d in $(MODULES); do \
		echo "== build $$d =="; \
		(cd $$d && go build ./...) || exit 1; \
	done

lint:
	@for d in $(MODULES); do \
		echo "== vet $$d =="; \
		(cd $$d && go vet ./...) || exit 1; \
	done

fmt:
	gofmt -l -w .
