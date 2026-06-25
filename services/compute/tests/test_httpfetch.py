import pytest
from compute import httpfetch


class FakeClock:
    def __init__(self): self.t = 0.0
    def __call__(self): return self.t


def test_cache_hit_within_ttl_does_not_refetch():
    clock = FakeClock()
    cache = httpfetch.TTLCache(ttl=100.0, clock=clock)
    calls = {"n": 0}
    def fetch():
        calls["n"] += 1
        return "v"
    assert cache.get_or_fetch("k", fetch) == "v"
    clock.t = 50.0
    assert cache.get_or_fetch("k", fetch) == "v"
    assert calls["n"] == 1  # served from cache


def test_cache_refetches_after_ttl_expiry():
    clock = FakeClock()
    cache = httpfetch.TTLCache(ttl=100.0, clock=clock)
    calls = {"n": 0}
    def fetch():
        calls["n"] += 1
        return calls["n"]
    assert cache.get_or_fetch("k", fetch) == 1
    clock.t = 101.0
    assert cache.get_or_fetch("k", fetch) == 2
    assert calls["n"] == 2


def test_cache_evicts_soonest_expiring_at_maxsize():
    clock = FakeClock()
    cache = httpfetch.TTLCache(ttl=100.0, maxsize=2, clock=clock)
    cache.get_or_fetch("a", lambda: "a")
    clock.t = 1.0
    cache.get_or_fetch("b", lambda: "b")
    clock.t = 2.0
    cache.get_or_fetch("c", lambda: "c")  # evicts "a" (soonest expiry)
    assert len(cache._data) == 2
    assert "a" not in cache._data


def test_fetch_json_caches_by_url_and_params(monkeypatch):
    seen = {"n": 0}
    class Resp:
        def raise_for_status(self): pass
        def json(self): return {"ok": True}
    def fake_get(url, params=None, headers=None, timeout=None):
        seen["n"] += 1
        return Resp()
    monkeypatch.setattr(httpfetch.requests, "get", fake_get)
    httpfetch._CACHE = httpfetch.TTLCache(ttl=100.0)  # isolate global
    a = httpfetch.fetch_json("http://x", params={"a": 1})
    b = httpfetch.fetch_json("http://x", params={"a": 1})
    assert a == b == {"ok": True}
    assert seen["n"] == 1  # second call cached


def test_fetch_json_raises_on_http_error(monkeypatch):
    class Resp:
        def raise_for_status(self): raise RuntimeError("502")
        def json(self): return {}
    monkeypatch.setattr(httpfetch.requests, "get", lambda *a, **k: Resp())
    httpfetch._CACHE = httpfetch.TTLCache(ttl=100.0)
    with pytest.raises(RuntimeError):
        httpfetch.fetch_json("http://x")
