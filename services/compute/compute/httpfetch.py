"""Cached HTTP egress for panel metrics.

Provides one process-global TTL cache and a ``fetch_json`` helper, injected
into the compute exec namespace by ``compute.endpoint.build_namespace``. The
cache key excludes the dashboard window so panels with different ranges share a
single fetched series. Network I/O happens outside the lock.
"""
from __future__ import annotations

import os
import threading
import time

import requests


class TTLCache:
    def __init__(self, ttl: float = 3600.0, maxsize: int = 256, clock=time.monotonic):
        self._ttl = ttl
        self._max = maxsize
        self._clock = clock
        self._lock = threading.Lock()
        self._data: dict = {}  # key -> (expires_at, value)

    def get_or_fetch(self, key, fetch):
        now = self._clock()
        with self._lock:
            hit = self._data.get(key)
            if hit and hit[0] > now:
                return hit[1]
        value = fetch()  # outside the lock — no serialization on slow I/O
        stored_at = self._clock()  # re-sample after fetch so TTL excludes fetch latency
        with self._lock:
            if len(self._data) >= self._max and key not in self._data:
                del self._data[min(self._data, key=lambda k: self._data[k][0])]
            self._data[key] = (stored_at + self._ttl, value)
        return value


_CACHE = TTLCache(ttl=float(os.environ.get("OC_COMPUTE_HTTP_TTL", "3600")))


def fetch_json(url, *, params=None, headers=None, ttl=None, timeout=15.0):
    # ``ttl`` is reserved for a future per-call cache override; not yet wired.
    key = (
        url,
        tuple(sorted((params or {}).items())),
        tuple(sorted((headers or {}).items())),
    )

    def _do():
        r = requests.get(url, params=params, headers=headers, timeout=timeout)
        r.raise_for_status()
        return r.json()

    return _CACHE.get_or_fetch(key, _do)
