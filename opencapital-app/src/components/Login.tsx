import { css, keyframes } from "@emotion/css";
import { GrafanaTheme2 } from "@grafana/data";
import { Alert, Button, Spinner, Text, useStyles2 } from "@grafana/ui";
import { Brand } from "./Brand";

type Props = {
  busy: boolean;
  error: string;
  onLogin: () => void;
  onDismissError: () => void;
};

export function Login({ busy, error, onLogin, onDismissError }: Props) {
  const styles = useStyles2(getStyles);

  return (
    <div className={styles.page}>
      <div className={styles.glow} />
      <div className={styles.card}>
        <Brand size="lg" />
        <div className={styles.copy}>
          <Text element="h1" variant="h3">
            Your market workspace, one launch away
          </Text>
          <Text color="secondary">
            Sign in to open your portfolios, instruments, and dashboards.
          </Text>
        </div>

        <Button
          size="lg"
          icon={busy ? undefined : "signin"}
          onClick={onLogin}
          disabled={busy}
          className={styles.cta}
        >
          {busy ? (
            <span className={styles.ctaBusy}>
              <Spinner inline /> Signing in…
            </span>
          ) : (
            "Log in to OpenCapital"
          )}
        </Button>

        {error && (
          <Alert
            title="Couldn't sign in"
            severity="error"
            onRemove={onDismissError}
            className={styles.alert}
          >
            {error}
          </Alert>
        )}
      </div>
      <Text variant="bodySmall" color="disabled">
        Secured by single sign-on
      </Text>
    </div>
  );
}

const float = keyframes({
  "0%": { transform: "translate(-50%, -50%) scale(1)", opacity: 0.55 },
  "100%": { transform: "translate(-50%, -52%) scale(1.08)", opacity: 0.85 },
});

const getStyles = (theme: GrafanaTheme2) => ({
  page: css({
    position: "relative",
    minHeight: "100vh",
    display: "flex",
    flexDirection: "column",
    alignItems: "center",
    justifyContent: "center",
    gap: theme.spacing(4),
    overflow: "hidden",
    background: theme.colors.background.canvas,
  }),
  glow: css({
    position: "absolute",
    top: "42%",
    left: "50%",
    width: 720,
    height: 720,
    transform: "translate(-50%, -50%)",
    background: `radial-gradient(circle, ${theme.colors.primary.main}22 0%, transparent 60%)`,
    filter: "blur(20px)",
    pointerEvents: "none",
    animation: `${float} 6s ease-in-out infinite alternate`,
  }),
  card: css({
    position: "relative",
    zIndex: 1,
    display: "flex",
    flexDirection: "column",
    alignItems: "center",
    gap: theme.spacing(3),
    padding: theme.spacing(5, 6),
    maxWidth: 460,
    textAlign: "center",
    background: theme.colors.background.primary,
    border: `1px solid ${theme.colors.border.weak}`,
    borderRadius: theme.shape.radius.default,
    boxShadow: theme.shadows.z3,
  }),
  copy: css({
    display: "flex",
    flexDirection: "column",
    gap: theme.spacing(1),
  }),
  cta: css({
    marginTop: theme.spacing(1),
    minWidth: 240,
    justifyContent: "center",
  }),
  ctaBusy: css({
    display: "inline-flex",
    alignItems: "center",
    gap: theme.spacing(1),
  }),
  alert: css({ marginTop: theme.spacing(1), textAlign: "left" }),
});
