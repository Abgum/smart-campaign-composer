COMPOSE := docker compose
COMPOSE_TEST := docker compose -f docker-compose.test.yml
GO_IMAGE := golang:1.26.10-bookworm
GO_RUN := docker run --rm -v $(CURDIR)/backend:/src/backend -v scc-go-mod-cache:/go/pkg/mod -w /src/backend $(GO_IMAGE)

.PHONY: up demo-data down logs reset test test-backend test-frontend test-local lint lint-go lint-ci tidy fmt volumes verify-geonames
ACTIONLINT_IMAGE := rhysd/actionlint:1.7.7
CI_WORKFLOW := .github/workflows/ci.yml

## up: build and start the whole app; the UI is at http://localhost:8080
up:
	$(COMPOSE) up --build -d
	@echo "UI and API:          http://localhost:8080"
	@echo "Mock provider stats: http://localhost:8090/stats"
	@echo "Worker metrics:      http://localhost:9091/metrics"

## demo-data: start the app and add six weeks of past demo campaigns (once).
## The seed run first waits for (or loads) the recipients the history needs.
demo-data: up
	$(COMPOSE) run --rm seed seed
	$(COMPOSE) run --rm seed history

## down: stop the app (data volumes are kept)
down:
	$(COMPOSE) down

## logs: follow the app's logs
logs:
	$(COMPOSE) logs -f api worker mockprovider

## reset: stop the app and delete its data (database, provider log)
reset:
	$(COMPOSE) down -v

## test: run every test suite in containers (no local Go or Node, no API key needed)
test: test-backend test-frontend

test-backend: volumes
	$(COMPOSE_TEST) run --rm backend-test; status=$$?; $(COMPOSE_TEST) down --remove-orphans >/dev/null 2>&1; exit $$status

test-frontend: volumes
	$(COMPOSE_TEST) run --rm --no-deps frontend-test; status=$$?; $(COMPOSE_TEST) down --remove-orphans >/dev/null 2>&1; exit $$status

## lint: format and static checks in containers (gofmt, go vet, the CI workflow);
## the frontend's type check (vue-tsc) runs with its tests
lint: lint-go lint-ci

lint-go: volumes
	$(GO_RUN) sh -c 'out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt would change:"; echo "$$out"; exit 1; fi'
	$(GO_RUN) go vet ./...

## lint-ci: the CI workflow is valid (actionlint) and keeps its contract: it
## runs make lint and make test, and needs no secret (no API key)
lint-ci:
	@test -f $(CI_WORKFLOW) || (echo "$(CI_WORKFLOW) is missing" && exit 1)
	docker run --rm -v $(CURDIR):/repo -w /repo $(ACTIONLINT_IMAGE) -color
	@grep -Eq '^[[:space:]]*run: make lint[[:space:]]*$$' $(CI_WORKFLOW) || (echo "CI must run make lint" && exit 1)
	@grep -Eq '^[[:space:]]*run: make test[[:space:]]*$$' $(CI_WORKFLOW) || (echo "CI must run make test" && exit 1)
	@! grep -n 'secrets\.' $(CI_WORKFLOW) || (echo "CI must not need a secret: the tests run without an API key" && exit 1)

## test-local: run backend tests with a local Go toolchain (needs TEST_DATABASE_URL for DB tests)
test-local:
	cd backend && go test -race -count=1 ./...

volumes:
	@docker volume create scc-go-mod-cache >/dev/null
	@docker volume create scc-go-build-cache >/dev/null
	@docker volume create scc-node-modules >/dev/null
	@docker volume create scc-npm-cache >/dev/null

tidy: volumes
	$(GO_RUN) go mod tidy

fmt: volumes
	$(GO_RUN) gofmt -w .

## verify-geonames: check catalog geonameIds against a GeoNames dump, e.g.
##   make verify-geonames GEONAMES_DUMP=$$HOME/Downloads/cities15000.txt
verify-geonames: volumes
	@test -n "$(GEONAMES_DUMP)" || (echo "GEONAMES_DUMP=/path/to/cities15000.txt required" && exit 2)
	$(COMPOSE_TEST) run --rm -v $(abspath $(GEONAMES_DUMP)):/data/geonames.txt:ro -e GEONAMES_DUMP=/data/geonames.txt \
		backend-test go test -count=1 -run TestCatalogMatchesGeoNamesDump -v ./internal/geo/; \
		status=$$?; $(COMPOSE_TEST) down --remove-orphans >/dev/null 2>&1; exit $$status
