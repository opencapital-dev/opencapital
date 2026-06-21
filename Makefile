SHELL := /bin/bash

# OpenCapital — fully-local desktop. This repo is the product + its data plane:
# the Tauri shell (opencapital-app), the data-plane services (services/), the
# shared Go libs (lib/), the event contract (schemas/), and the data-plane
# definitions (dataplane/: postgres schema, RisingWave DDL, WSL supervisor).
# Cloud operations, secrets (SOPS), and plugin gating live in the private repo.

PYTEST ?= .venv/bin/python -m pytest

.PHONY: help test test-unit \
        compute-venv compute-freeze compute-smoke compute-sidecar-stage \
        grafana-ui grafana-ui-build grafana-ui-dev grafana-overlay-pull \
        dataplane-resource dataplane-stage artifacts-stage \
        app app-stage app-deps signing-key \
        rw-apply rw-parity schemas install-local

help:
	@echo "Build the desktop app (the ONE path — same steps CI runs; build it, run from the build):"
	@echo "  app                       - build the self-contained bundle (.app + .dmg) like CI"
	@echo "  app-stage                 - stage all bundled prereqs only (no build)"
	@echo ""
	@echo "Individual build prereqs (invoked by app-stage; CI calls the same targets):"
	@echo "  compute-venv              - create .venv + install the compute freeze deps"
	@echo "  compute-sidecar-stage     - freeze compute + stage it as the Tauri externalBin sidecar"
	@echo "  dataplane-stage           - stage the dataplane/ tree (postgres init SQL + risingwave DDL)"
	@echo "  artifacts-stage           - download Postgres/RisingWave/Grafana tarballs into the bundle"
	@echo "  grafana-overlay-pull      - pull the pinned Grafana overlay (grafana-overlay.pin) into the bundle"
	@echo "  app-deps                  - npm ci for the shell frontend"
	@echo "  signing-key               - ensure an updater signing key (CI secret, else local throwaway)"
	@echo ""
	@echo "Other:"
	@echo "  test / test-unit          - python unit tests (compute; no data plane required)"
	@echo "  compute-freeze / -smoke   - freeze / smoke-test the frozen compute binary"
	@echo "  grafana-ui / -dev         - OPT-IN (fork devs): build the overlay from ../grafana source"
	@echo "  rw-apply / rw-parity      - apply local RisingWave DDL / run the parity check"
	@echo "  schemas                   - register Avro schemas against a local registry"

# ── Python unit tests ──────────────────────────────────────────────────
test test-unit:
	@$(PYTEST) services/compute/tests -q -m "not integration and not freeze"; rc=$$?; [ $$rc -eq 0 ] || [ $$rc -eq 5 ]

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

# ── dataplane tree (postgres init SQL + risingwave DDL/apply.sh) ───────
# Staged as a Tauri resource so a downloaded app can bootstrap without the repo
# checkout. macOS/Linux only — Windows runs the plane via the bundled WSL distro.
DATAPLANE_RESOURCE_DST := ./$(APP_DIR)/src-tauri/resources/dataplane

dataplane-resource: ## Stage dataplane/ (postgres init SQL + risingwave DDL/apply.sh) as a Tauri resource
	rm -rf $(DATAPLANE_RESOURCE_DST)
	mkdir -p $(DATAPLANE_RESOURCE_DST)
	cp -R dataplane/postgres $(DATAPLANE_RESOURCE_DST)/postgres
	cp -R dataplane/risingwave $(DATAPLANE_RESOURCE_DST)/risingwave
	@echo ">>> dataplane tree staged at $(DATAPLANE_RESOURCE_DST)"

dataplane-stage: dataplane-resource ## Stage the dataplane tree for the bundle

# ── Bundled data-plane artifacts (Postgres / RisingWave / Grafana) ─────
# Downloaded into the bundle resources so the .app is fully self-contained: the
# app extracts these at first launch instead of ever downloading (see
# runtime::resolve_artifact, which fails loudly if they are absent). Cached by
# the staged file's presence. macOS arm64 URLs — postgres/risingwave URLs match
# config.rs default_{postgres,risingwave}_url; grafana matches GRAFANA_VERSION.
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

# ── Desktop app (build) ────────────────────────────────────────────────
# `make app` is the ONE build path — the same steps CI runs. There is no dev
# mode: to test, build the bundle and run it from the build. app-stage stages
# every bundled prereq (compute + Go service sidecars, dataplane tree, pinned
# Grafana overlay, npm deps, and the PG/RW/Grafana artifact tarballs); the
# macOS bundle config (tauri.macos.conf.json) is auto-merged by tauri, so the
# build is a plain `tauri build` with no extra flags.
$(APP_DIR)/node_modules: $(APP_DIR)/package-lock.json
	cd $(APP_DIR) && npm ci

app-deps: $(APP_DIR)/node_modules ## npm ci for the shell frontend

app-stage: $(COMPUTE_SIDECAR) dataplane-stage artifacts-stage grafana-overlay-pull app-deps ## Stage all bundled prereqs
	@echo ">>> app staged (sidecars + dataplane + artifacts + overlay + deps) — ready to build"

# tauri.conf.json has createUpdaterArtifacts=true + an updater pubkey, so the
# build signs the updater artifact and needs a private key. CI injects the prod
# key via the TAURI_SIGNING_PRIVATE_KEY secret; locally we generate a throwaway
# key (gitignored) so `make app` is the same command and completes the same way.
# The locally-signed updater artifact is never consumed (you run the .dmg).
TAURI_SIGNING_KEY := $(APP_DIR)/.tauri-signing.key

signing-key: ## Ensure an updater signing key exists (env secret in CI, throwaway file locally)
	@if [ -n "$$TAURI_SIGNING_PRIVATE_KEY" ]; then \
	  echo ">>> using TAURI_SIGNING_PRIVATE_KEY from the environment"; \
	elif [ ! -f $(TAURI_SIGNING_KEY) ]; then \
	  echo ">>> generating local throwaway updater signing key"; \
	  (cd $(APP_DIR) && npx tauri signer generate --ci -p "" -w $(CURDIR)/$(TAURI_SIGNING_KEY)); \
	fi

app: app-stage signing-key ## Build the self-contained desktop bundle (.app + .dmg), mirroring CI
	@cd $(APP_DIR) && \
	  export TAURI_SIGNING_PRIVATE_KEY="$${TAURI_SIGNING_PRIVATE_KEY:-$$(cat $(CURDIR)/$(TAURI_SIGNING_KEY))}" && \
	  export TAURI_SIGNING_PRIVATE_KEY_PASSWORD="$${TAURI_SIGNING_PRIVATE_KEY_PASSWORD-}" && \
	  n=0; until npx tauri build --bundles app,dmg; do \
	    n=$$((n+1)); \
	    [ $$n -ge 3 ] && { echo "tauri build failed after $$n attempts"; exit 1; }; \
	    echo "build attempt $$n failed (likely flaky bundle_dmg.sh) — retrying"; sleep 5; \
	  done

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
