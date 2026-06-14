package manifest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func serve(t *testing.T, body string, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const samplePlugin = `{
  "schemaVersion": 1,
  "pluginId": "acme-charting",
  "publisher": "Acme Corp",
  "registry": {
    "host": "ghcr.io",
    "namespace": "acme/oc-plugins",
    "stagingNamespace": "acme/oc-plugins-staging",
    "publicURL": "https://ghcr.io"
  },
  "versions": ["1.4.0", "1.3.0"],
  "preview": ["1.5.0-rc1"]
}`

func TestPluginClientParses(t *testing.T) {
	c := NewPluginClient(serve(t, samplePlugin, 200), nil, 0, nil)
	m, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if m.PluginID != "acme-charting" || m.Publisher != "Acme Corp" {
		t.Fatalf("bad meta: %+v", m)
	}
	if m.Registry.Namespace != "acme/oc-plugins" || m.Registry.StagingNamespace != "acme/oc-plugins-staging" {
		t.Fatalf("bad registry: %+v", m.Registry)
	}
	if len(m.Versions) != 2 || m.Versions[0] != "1.4.0" {
		t.Fatalf("bad versions: %v", m.Versions)
	}
}

func TestPluginClientValidation(t *testing.T) {
	cases := map[string]string{
		"no pluginId":        `{"schemaVersion":1,"registry":{"host":"h","namespace":"a/b"},"versions":[]}`,
		"no host":            `{"schemaVersion":1,"pluginId":"x","registry":{"namespace":"a/b"},"versions":[]}`,
		"no namespace":       `{"schemaVersion":1,"pluginId":"x","registry":{"host":"h"},"versions":[]}`,
		"preview no staging": `{"schemaVersion":1,"pluginId":"x","registry":{"host":"h","namespace":"a/b"},"versions":[],"preview":["1.0.0"]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c := NewPluginClient(serve(t, body, 200), nil, 0, nil)
			if _, err := c.Fetch(context.Background()); err == nil {
				t.Fatalf("expected validation error for %q", name)
			}
		})
	}
}

const sampleList = `{
  "schemaVersion": 1,
  "plugins": [
    "https://example.test/core-app.json",
    "https://example.test/core-datasource.json"
  ]
}`

func TestListClientParses(t *testing.T) {
	c := NewListClient(serve(t, sampleList, 200), nil, 0, nil)
	urls, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(urls) != 2 || urls[0] != "https://example.test/core-app.json" {
		t.Fatalf("bad urls: %v", urls)
	}
}

func TestListClientRejectsNonURL(t *testing.T) {
	c := NewListClient(serve(t, `{"plugins":["not a url"]}`, 200), nil, 0, nil)
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("expected validation error for non-URL entry")
	}
}
