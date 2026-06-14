SHELL := /bin/bash

# OpenCapital — fully-local desktop. This repo is the product + its data plane:
# the Tauri shell (opencapital-app), the data-plane services (services/), the
# shared Go libs (lib/), the event contract (schemas/), and the data-plane
# definitions (dataplane/: postgres schema, RisingWave DDL, WSL supervisor).
# Cloud operations, secrets (SOPS), and plugin gating live in the private repo.

PYTEST ?= .venv/bin/python -m pytest
GO_SVCS := control-plane gateway read-gateway

.PHONY: help test test-unit go-build go-test \
        compute-venv compute-freeze compute-smoke compute-sidecar-stage \
        grafana-ui grafana-ui-build grafana-ui-dev grafana-overlay-pull \
        go-sidecars dataplane-resource dataplane-stage artifacts-stage \
        app dev app-stage app-deps \
        rw-apply rw-parity schemas install-local

help:
	@echo "Build the desktop app (same steps CI runs — one command):"
	@echo "  app                       - build the bundle (.app + .dmg) like CI"
	@echo "  dev                       - run the app in dev mode (same staged prereqs)"
	@echo "  app-stage                 - stage build prereqs only (sidecar + overlay + npm deps)"
	@echo ""
	@echo "Individual build prereqs (invoked by app-stage; CI calls the same targets):"
	@echo "  compute-venv              - create .venv + install the compute freeze deps"
	@echo "  compute-sidecar-stage     - freeze compute + stage it as the Tauri externalBin sidecar"
	@echo "  dataplane-stage           - build the 3 Go service sidecars + stage the dataplane/ tree"
	@echo "  artifacts-stage           - download Postgres/RisingWave/Grafana tarballs into the bundle (release only)"
	@echo "  grafana-overlay-pull      - pull the pinned Grafana overlay (grafana-overlay.pin) into the bundle"
	@echo "  app-deps                  - npm ci for the shell frontend"
	@echo ""
	@echo "Other:"
	@echo "  test / test-unit          - python unit tests (compute; no data plane required)"
	@echo "  go-build / go-test        - build / test the Go data-plane services"
	@echo "  compute-freeze / -smoke   - freeze / smoke-test the frozen compute binary"
	@echo "  grafana-ui / -dev         - OPT-IN (fork devs): build the overlay from ../grafana source"
	@echo "  rw-apply / rw-parity      - apply local RisingWave DDL / run the parity check"
	@echo "  schemas                   - register Avro schemas against a local registry"

# ── Python unit tests ──────────────────────────────────────────────────
test test-unit:
	@$(PYTEST) services/compute/tests -q -m "not integration and not freeze"; rc=$$?; [ $$rc -eq 0 ] || [ $$rc -eq 5 ]

# ── Go data-plane services ─────────────────────────────────────────────
go-build:
	@for s in $(GO_SVCS); do echo ">>> build $$s"; (cd services/$$s && go build ./... && go vet ./...) || exit 1; done

go-test:
	@for s in $(GO_SVCS); do echo ">>> test $$s"; (cd services/$$s && go test ./...) || exit 1; done

# ── App build paths ────────────────────────────────────────────────────
APP_DIR := opencapital-app

# ── Grafana UI overlay (desktop) ───────────────────────────────────────
# The Tauri app downloads vanilla Grafana and overlays our customized frontend
# (nav customization is frontend-only). The shipped overlay is a pinned release
# asset built in the fork (../grafana); `grafana-overlay-pull` fetches it — this
# is the default build path and matches CI exactly. The `grafana-ui*` targets
# below build the overlay from fork source and are opt-in for fork frontend devs.
GRAFANA_FORK_DIR    := ../grafana
GRAFANA_OVERLAY_DST := ./$(APP_DIR)/src-tauri/resources/grafana-public
GRAFANA_OVERLAY_PIN := ./$(APP_DIR)/grafana-overlay.pin
GRAFANA_OVERLAY_REPO := opencapital-dev/grafana
OPENCAPITAL_GRAFANA := $(HOME)/.opencapital/runtime/grafana

grafana-overlay-pull: ## Pull the pinned Grafana overlay (CI parity) into the app bundle
	@pin=$$(tr -d '[:space:]' < $(GRAFANA_OVERLAY_PIN)); \
	dst=$(GRAFANA_OVERLAY_DST); \
	if [ -f "$$dst/.overlay-version" ] && [ "$$(cat $$dst/.overlay-version)" = "$$pin" ]; then \
	  echo ">>> overlay $$pin already staged"; exit 0; fi; \
	echo ">>> pulling overlay $$pin from $(GRAFANA_OVERLAY_REPO)"; \
	tmp=$$(mktemp -d); \
	gh release download "$$pin" --repo $(GRAFANA_OVERLAY_REPO) --pattern grafana-overlay.tar.gz --dir "$$tmp" \
	  || { echo "overlay download failed (gh auth + access to $(GRAFANA_OVERLAY_REPO)?)"; rm -rf "$$tmp"; exit 1; }; \
	rm -rf "$$dst"; mkdir -p "$$dst"; \
	tar xzf "$$tmp/grafana-overlay.tar.gz" -C "$$dst"; \
	echo "$$pin" > "$$dst/.overlay-version"; \
	rm -rf "$$tmp"; \
	echo ">>> overlay $$pin staged at $$dst"

grafana-ui-build:
	cd $(GRAFANA_FORK_DIR) && yarn install --immutable && yarn build

grafana-ui: grafana-ui-build ## OPT-IN (fork devs): build overlay from ../grafana source + stage into the app bundle
	rm -rf $(GRAFANA_OVERLAY_DST)
	mkdir -p $(GRAFANA_OVERLAY_DST)
	cp -R $(GRAFANA_FORK_DIR)/public/build $(GRAFANA_OVERLAY_DST)/build
	cp -R $(GRAFANA_FORK_DIR)/public/views $(GRAFANA_OVERLAY_DST)/views
	git -C $(GRAFANA_FORK_DIR) rev-parse --short HEAD > $(GRAFANA_OVERLAY_DST)/.overlay-version
	@echo ">>> overlay staged at $(GRAFANA_OVERLAY_DST)"

grafana-ui-dev: grafana-ui-build ## Build fork frontend + overlay onto the running app's grafana home
	@test -d $(OPENCAPITAL_GRAFANA) || { echo "no grafana home at $(OPENCAPITAL_GRAFANA) — launch Grafana once in OpenCapital first"; exit 1; }
	rm -rf $(OPENCAPITAL_GRAFANA)/public/build && cp -R $(GRAFANA_FORK_DIR)/public/build $(OPENCAPITAL_GRAFANA)/public/build
	rm -rf $(OPENCAPITAL_GRAFANA)/public/views && cp -R $(GRAFANA_FORK_DIR)/public/views $(OPENCAPITAL_GRAFANA)/public/views
	@echo ">>> overlay applied — relaunch Grafana in OpenCapital"

# ── compute sidecar binary ─────────────────────────────────────────────
# compute-venv mirrors CI: a .venv with the freeze deps (polars + pyinstaller).
# Idempotent — skips if the venv already has pyinstaller.
compute-venv: ## Create .venv + install the compute freeze deps (polars, pyinstaller)
	@test -x .venv/bin/pyinstaller || { \
	  echo ">>> creating .venv ($$(python3 --version)) + installing freeze deps"; \
	  python3 -m venv .venv && \
	  .venv/bin/python -m pip install --quiet --upgrade pip && \
	  .venv/bin/pip install --quiet -r services/compute/freeze-requirements.txt; }

COMPUTE_DIST        := services/compute/dist/compute
COMPUTE_SIDECAR_DIR := ./$(APP_DIR)/src-tauri/binaries
HOST_TRIPLE         := $(shell rustc -vV | sed -n 's/^host: //p')
COMPUTE_SIDECAR     := $(COMPUTE_SIDECAR_DIR)/compute-$(HOST_TRIPLE)

# Freeze only when the source or spec changed (or dist is missing). compute-venv
# is order-only (|): present the venv without forcing a re-freeze on venv mtime.
$(COMPUTE_DIST): services/compute/main.py services/compute/compute.spec $(wildcard services/compute/compute/*.py) | compute-venv
	cd services/compute && $(CURDIR)/.venv/bin/pyinstaller compute.spec --distpath dist --workpath /tmp/compute-build --noconfirm

$(COMPUTE_SIDECAR): $(COMPUTE_DIST)
	mkdir -p $(COMPUTE_SIDECAR_DIR)
	cp $(COMPUTE_DIST) $@
	chmod +x $@
	@echo ">>> compute sidecar staged at $@"

compute-freeze: $(COMPUTE_DIST) ## Freeze compute to a one-file binary (services/compute/dist/compute)

compute-smoke: compute-freeze
	$(PYTEST) services/compute/tests/test_freeze_smoke.py -v -m freeze

compute-sidecar-stage: $(COMPUTE_SIDECAR) ## Freeze + stage the compute binary as the Tauri externalBin sidecar

# ── Go data-plane service sidecars + dataplane tree ────────────────────
# Bundled the same way as the compute sidecar: the 3 Go services are built per
# host triple and staged as Tauri externalBin (binaries/<name>-<triple>), then
# resolved at runtime next to the app exe (see dataplane.rs ensure_service). The
# dataplane/ tree (postgres init SQL + risingwave DDL/apply.sh) is staged as a
# Tauri resource so a downloaded app can bootstrap without the repo checkout.
# macOS/Linux only — Windows runs the plane via the bundled WSL distro.
DATAPLANE_RESOURCE_DST := ./$(APP_DIR)/src-tauri/resources/dataplane

go-sidecars: ## Build the 3 Go data-plane services as Tauri externalBin sidecars
	@mkdir -p $(COMPUTE_SIDECAR_DIR)
	@for s in $(GO_SVCS); do \
	  echo ">>> building $$s sidecar"; \
	  (cd services/$$s && go build -o $(CURDIR)/$(COMPUTE_SIDECAR_DIR)/$$s-$(HOST_TRIPLE) ./cmd/$$s) || exit 1; \
	  chmod +x $(COMPUTE_SIDECAR_DIR)/$$s-$(HOST_TRIPLE); \
	done
	@echo ">>> Go sidecars staged in $(COMPUTE_SIDECAR_DIR)"

dataplane-resource: ## Stage dataplane/ (postgres init SQL + risingwave DDL/apply.sh) as a Tauri resource
	rm -rf $(DATAPLANE_RESOURCE_DST)
	mkdir -p $(DATAPLANE_RESOURCE_DST)
	cp -R dataplane/postgres $(DATAPLANE_RESOURCE_DST)/postgres
	cp -R dataplane/risingwave $(DATAPLANE_RESOURCE_DST)/risingwave
	@echo ">>> dataplane tree staged at $(DATAPLANE_RESOURCE_DST)"

dataplane-stage: go-sidecars dataplane-resource ## Build Go sidecars + stage the dataplane tree for the bundle

# ── Bundled data-plane artifacts (Postgres / RisingWave / Grafana) ─────
# Downloaded into the bundle resources so a release .app is fully offline (the
# app extracts these instead of downloading on first launch — see
# runtime::resolve_artifact). Staged only for `make app`/CI release builds, NOT
# `make dev` (dev keeps the lighter download-to-~/.opencapital path). macOS arm64
# URLs — keep in sync with config.rs default_{postgres,risingwave,grafana}_url.
ARTIFACTS_DST  := ./$(APP_DIR)/src-tauri/resources/artifacts
POSTGRES_URL   := https://github.com/theseus-rs/postgresql-binaries/releases/download/17.10.0/postgresql-17.10.0-aarch64-apple-darwin.tar.gz
RISINGWAVE_URL := https://github.com/opencapital-dev/opencapital/releases/download/risingwave-2.8.0-macos-arm64/risingwave-2.8.0-macos-arm64.tar.gz
GRAFANA_URL    := https://dl.grafana.com/oss/release/grafana-13.0.2.darwin-arm64.tar.gz

artifacts-stage: ## Download Postgres/RisingWave/Grafana tarballs into the bundle resources (cached)
	@mkdir -p $(ARTIFACTS_DST)
	@for pair in "postgres.tar.gz=$(POSTGRES_URL)" "risingwave.tar.gz=$(RISINGWAVE_URL)" "grafana.tar.gz=$(GRAFANA_URL)"; do \
	  name=$${pair%%=*}; url=$${pair#*=}; \
	  if [ -s "$(ARTIFACTS_DST)/$$name" ]; then echo ">>> $$name already staged"; continue; fi; \
	  echo ">>> downloading $$name"; \
	  curl -fL --retry 3 -o "$(ARTIFACTS_DST)/$$name" "$$url" || { echo "download $$name failed"; rm -f "$(ARTIFACTS_DST)/$$name"; exit 1; }; \
	done
	@echo ">>> artifacts staged in $(ARTIFACTS_DST)"

# ── Desktop app (build / run) ──────────────────────────────────────────
# `app` and `dev` are the two entry points. Both stage the exact prereqs CI
# stages (compute sidecar, pinned overlay, npm deps) then build/run. The
# prereqs are idempotent, so repeat `make dev` runs are fast.
$(APP_DIR)/node_modules: $(APP_DIR)/package-lock.json
	cd $(APP_DIR) && npm ci

app-deps: $(APP_DIR)/node_modules ## npm ci for the shell frontend

app-stage: $(COMPUTE_SIDECAR) dataplane-stage grafana-overlay-pull app-deps ## Stage all build prereqs (sidecars + dataplane + overlay + deps)
	@echo ">>> app staged (sidecars + dataplane + overlay + deps) — ready to build/run"

app: app-stage artifacts-stage ## Build the offline desktop bundle (.app + .dmg, bundles PG/RW/Grafana), mirroring CI
	@cd $(APP_DIR) && n=0; until npx tauri build --config $(CURDIR)/$(APP_DIR)/src-tauri/tauri.bundle.conf.json --bundles app,dmg; do \
	  n=$$((n+1)); \
	  [ $$n -ge 3 ] && { echo "tauri build failed after $$n attempts"; exit 1; }; \
	  echo "build attempt $$n failed (likely flaky bundle_dmg.sh) — retrying"; sleep 5; \
	done

dev: app-stage ## Run the app in dev mode (same staged prereqs as the bundle)
	cd $(APP_DIR) && npm run tauri dev

# ── RisingWave (local data plane) ──────────────────────────────────────
rw-apply:
	bash dataplane/risingwave/apply.sh

rw-parity:
	.venv/bin/python scripts/risingwave_parity.py

schemas:
	REGISTRY_URL=http://localhost:8081 SCHEMAS_DIR=$(PWD)/schemas \
	  bash scripts/register-schemas.sh

install-local:
	python -m pip install -r requirements-local.txt
