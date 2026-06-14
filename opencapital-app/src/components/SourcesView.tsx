import { useEffect, useState } from "react";
import { css } from "@emotion/css";
import { GrafanaTheme2 } from "@grafana/data";
import {
  Alert,
  Badge,
  Button,
  Card,
  EmptyState,
  Field,
  Input,
  Spinner,
  Stack,
  Text,
  useStyles2,
} from "@grafana/ui";
import { api, errMsg } from "../api";
import type { PluginSource } from "../types";

export function SourcesView() {
  const styles = useStyles2(getStyles);
  const [sources, setSources] = useState<PluginSource[]>([]);
  const [loading, setLoading] = useState(true);
  const [url, setUrl] = useState("");
  const [adding, setAdding] = useState(false);
  const [error, setError] = useState("");

  async function load() {
    setLoading(true);
    setError("");
    try {
      setSources((await api.listSources()) ?? []);
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setLoading(false);
    }
  }

  useEffect(() => {
    load();
  }, []);

  async function add() {
    const u = url.trim();
    if (!u) return;
    setAdding(true);
    setError("");
    try {
      await api.addSource(u);
      setUrl("");
      await load();
    } catch (e) {
      setError(errMsg(e));
    } finally {
      setAdding(false);
    }
  }

  async function remove(manifestUrl: string) {
    setError("");
    try {
      await api.removeSource(manifestUrl);
      await load();
    } catch (e) {
      setError(errMsg(e));
    }
  }

  return (
    <div className={styles.wrap}>
      <header>
        <Text element="h1" variant="h3">
          Plugin sources
        </Text>
        <Text color="secondary">
          Add third-party plugins by their manifest URL. Official plugins are
          always available and don't need a source.
        </Text>
      </header>

      <Alert title="Only add sources you trust" severity="warning">
        Installing a plugin runs its code on your machine. A source URL you add is
        unverified — it can point at any registry. Add only manifests from authors
        you trust.
      </Alert>

      <Card>
        <Card.Heading>Add a source</Card.Heading>
        <Card.Description>
          <Field label="Per-plugin manifest URL">
            <Stack direction="row" gap={1}>
              <Input
                value={url}
                width={60}
                placeholder="https://example.com/my-plugin.json"
                onChange={(e) => setUrl(e.currentTarget.value)}
                onKeyDown={(e) => e.key === "Enter" && add()}
              />
              <Button icon="plus" disabled={adding || !url.trim()} onClick={add}>
                {adding ? "Validating…" : "Add"}
              </Button>
            </Stack>
          </Field>
        </Card.Description>
      </Card>

      {error && (
        <Alert title="Source error" severity="error" onRemove={() => setError("")}>
          {error}
        </Alert>
      )}

      {loading ? (
        <div className={styles.center}>
          <Spinner size="lg" />
        </div>
      ) : sources.length === 0 ? (
        <EmptyState variant="completed" message="No third-party sources added." />
      ) : (
        <Stack direction="column" gap={1}>
          {sources.map((s) => (
            <Card key={s.manifest_url}>
              <Card.Heading>{s.publisher || "Unknown publisher"}</Card.Heading>
              <Card.Meta>{s.manifest_url}</Card.Meta>
              <Card.Tags>
                <Badge text="Third-party" color="orange" icon="exclamation-triangle" />
              </Card.Tags>
              <Card.Actions>
                <Button
                  variant="destructive"
                  fill="outline"
                  icon="trash-alt"
                  onClick={() => remove(s.manifest_url)}
                >
                  Remove
                </Button>
              </Card.Actions>
            </Card>
          ))}
        </Stack>
      )}
    </div>
  );
}

const getStyles = (theme: GrafanaTheme2) => ({
  wrap: css({
    display: "flex",
    flexDirection: "column",
    gap: theme.spacing(2),
    width: "100%",
    maxWidth: 820,
    margin: "0 auto",
  }),
  center: css({
    display: "flex",
    justifyContent: "center",
    padding: theme.spacing(6),
  }),
});
