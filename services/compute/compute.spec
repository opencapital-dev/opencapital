# -*- mode: python ; coding: utf-8 -*-
# PyInstaller spec for the compute sidecar binary.
#
# Build:
#   cd services/compute
#   pyinstaller compute.spec
#
# Output: services/compute/dist/compute  (one-file, current platform)
# One-file extraction cold-start is ~7s on dev hardware; the Tauri sidecar
# health-poll timeout (Task 2) must exceed this.
#
# Polars packaging notes:
#   polars >=1.28 (tested 1.41) splits the Rust extension into a separate wheel
#   (_polars_runtime_32 / _polars_runtime_64 / _polars_runtime_compat).
#   The _plr.py shim tries each at import time; we must collect all that
#   are installed so the frozen binary finds them at runtime.  On this
#   machine only _polars_runtime_32 is installed.
#
# numpy/pandas/scipy/pyarrow are excluded: polars imports them lazily for
# optional interop (.to_pandas()/.to_numpy()/.to_arrow()) the service never
# calls, so PyInstaller's static trace would otherwise bundle ~30-40MB of
# dead weight.

from PyInstaller.utils.hooks import collect_all, collect_submodules

_compute_pkg = SPECPATH  # services/compute/ — compute/ lives here

polars_datas, polars_binaries, polars_hiddenimports = collect_all('polars')
rt32_datas, rt32_binaries, rt32_hiddenimports = collect_all('_polars_runtime_32')

a = Analysis(
    ['main.py'],
    pathex=[_compute_pkg],
    binaries=polars_binaries + rt32_binaries,
    datas=polars_datas + rt32_datas,
    hiddenimports=(
        polars_hiddenimports
        + rt32_hiddenimports
        + collect_submodules('compute')
        + [
            '_polars_runtime_32',
            '_polars_runtime_32._polars_runtime',
            '_polars_runtime_32.build_feature_flags',
            'polars._plr',
            'polars._cpu_check',
        ]
    ),
    hookspath=[],
    hooksconfig={},
    runtime_hooks=[],
    excludes=['pandas', 'scipy', 'numpy', 'pyarrow'],
    noarchive=False,
    optimize=0,
)

pyz = PYZ(a.pure)

exe = EXE(
    pyz,
    a.scripts,
    a.binaries,
    a.datas,
    [],
    name='compute',
    debug=False,
    bootloader_ignore_signals=False,
    strip=False,
    upx=False,
    upx_exclude=[],
    runtime_tmpdir=None,
    console=True,
    disable_windowed_traceback=False,
    argv_emulation=False,
    target_arch=None,
    codesign_identity=None,
    entitlements_file=None,
    onefile=True,
)
