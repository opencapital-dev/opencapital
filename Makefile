SHELL := /bin/bash

# OpenCapital — fully-local desktop. This repo is the product + its data plane:
# the Tauri shell (opencapital-app), the data-plane services (services/), the
# shared Go libs (lib/), the event contract (schemas/), and the data-plane
# definitions (dataplane/: postgres schema, RisingWave DDL, WSL supervisor).
# Cloud operations, secrets (SOPS), and plugin gating live in the private repo.

PYTEST ?= .venv/bin/python -m pytest
GO_SVCS := control-plane gateway read-gateway

.PHONY: help test test-unit go-build go-test \
        compute-freeze compute-smoke compute-sidecar-stage \
        grafana-ui grafana-ui-build grafana-ui-dev \
        rw-apply rw-parity schemas install-local

help:
	@echo "Targets:"
	@echo "  test / test-unit          - python unit tests (compute; no data plane required)"
	@echo "  go-build / go-test        - build / test the Go data-plane services"
	@echo "  compute-freeze            - freeze compute to a one-file binary (services/compute/dist/compute)"
	@echo "  compute-smoke             - build + smoke-test the frozen compute binary"
	@echo "  compute-sidecar-stage     - stage the frozen compute binary as the Tauri externalBin sidecar"
	@echo "  grafana-ui / -dev         - build the fork frontend + stage/overlay it for the app"
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

# ── Grafana UI overlay (desktop) ───────────────────────────────────────
# The Tauri app downloads vanilla Grafana and overlays our customized frontend
# (nav customization is frontend-only). The fork lives at ../grafana.
GRAFANA_FORK_DIR    := ../grafana
GRAFANA_OVERLAY_DST := ./opencapital-app/src-tauri/resources/grafana-public
OPENCAPITAL_GRAFANA := $(HOME)/.opencapital/runtime/grafana

grafana-ui-build:
	cd $(GRAFANA_FORK_DIR) && yarn install --immutable && yarn build

grafana-ui: grafana-ui-build ## Build fork frontend + stage the overlay into the app bundle
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
compute-freeze:
	cd services/compute && $(CURDIR)/.venv/bin/pyinstaller compute.spec --distpath dist --workpath /tmp/compute-build --noconfirm

compute-smoke: compute-freeze
	$(PYTEST) services/compute/tests/test_freeze_smoke.py -v -m freeze

COMPUTE_SIDECAR_DIR := ./opencapital-app/src-tauri/binaries
HOST_TRIPLE         := $(shell rustc -vV | sed -n 's/^host: //p')

compute-sidecar-stage: compute-freeze ## Freeze + stage the compute binary as the Tauri externalBin sidecar
	mkdir -p $(COMPUTE_SIDECAR_DIR)
	cp services/compute/dist/compute $(COMPUTE_SIDECAR_DIR)/compute-$(HOST_TRIPLE)
	chmod +x $(COMPUTE_SIDECAR_DIR)/compute-$(HOST_TRIPLE)
	@echo ">>> compute sidecar staged at $(COMPUTE_SIDECAR_DIR)/compute-$(HOST_TRIPLE)"

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
