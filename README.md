# OpenCapital

Fully-local desktop portfolio analytics. The entire data plane runs on the
user's own machine — market data, portfolio state, and broker credentials never
leave it. The only remote dependencies are login/licensing (Kinde) and the
plugin marketplace at install/update time. Runtime is fully offline.

## Layout

| Path | What |
|---|---|
| `opencapital-app/` | Tauri desktop shell (Rust) + frontend; supervises the local data plane and embeds Grafana |
| `services/control-plane` | identity, portfolios/instruments, plugin marketplace client (Go) |
| `services/gateway` | ingestion authority; validates + writes events into RisingWave (Go) |
| `services/read-gateway` | sole RisingWave reader; serves the query DSL to plugins (Go) |
| `services/compute` | Python analytics sidecar (frozen via PyInstaller) |
| `lib/` | shared Go libraries (datakey, jwks, instance-bootstrap) |
| `schemas/` | Avro event contract (`portfolio_events.v2`, `data.v2`) |
| `dataplane/` | data-plane definitions the app applies at runtime: postgres init schema, RisingWave DDL, WSL supervisor |

## Platforms

- **macOS** — native processes (data plane + shell), no VM.
- **Windows** — one bundled WSL2 distro runs the Linux data-plane binaries
  (RisingWave has no Windows binary); the shell runs natively.

## Build

```
make go-build           # build + vet the Go services
make test-unit          # python unit tests (compute)
make compute-sidecar-stage   # freeze compute + stage it as a Tauri sidecar
make grafana-ui         # build the Grafana fork frontend + stage the overlay
```

Releases (installers + `plugins.json` + `latest.json`) are published from this
repo. Cloud operations, secrets, and plugin promotion/signing live in the
private `portfolio-management-v2` repo.
