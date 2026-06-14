import { css } from "@emotion/css";
import { GrafanaTheme2 } from "@grafana/data";
import { useStyles2 } from "@grafana/ui";

type Props = {
  size?: "sm" | "lg";
};

/** OpenCapital wordmark + a small candlestick glyph drawn in brand blue. */
export function Brand({ size = "sm" }: Props) {
  const styles = useStyles2(getStyles);
  const px = size === "lg" ? 34 : 22;

  return (
    <div className={styles.wrap}>
      <svg width={px} height={px} viewBox="0 0 24 24" fill="none" aria-hidden>
        <rect x="4" y="9" width="3" height="9" rx="1" className={styles.barLow} />
        <rect x="4" y="6" width="3" height="3" rx="1" className={styles.wick} />
        <rect x="10.5" y="4" width="3" height="11" rx="1" className={styles.barHigh} />
        <rect x="10.5" y="15" width="3" height="3" rx="1" className={styles.wick} />
        <rect x="17" y="11" width="3" height="7" rx="1" className={styles.barLow} />
        <rect x="17" y="8" width="3" height="3" rx="1" className={styles.wick} />
      </svg>
      <span className={size === "lg" ? styles.wordLg : styles.word}>
        Tick<span className={styles.accent}>Viewer</span>
      </span>
    </div>
  );
}

const getStyles = (theme: GrafanaTheme2) => ({
  wrap: css({
    display: "inline-flex",
    alignItems: "center",
    gap: theme.spacing(1),
    userSelect: "none",
  }),
  barHigh: css({ fill: theme.colors.primary.main }),
  barLow: css({ fill: theme.colors.primary.border }),
  wick: css({ fill: theme.colors.text.secondary, opacity: 0.5 }),
  word: css({
    fontSize: theme.typography.h5.fontSize,
    fontWeight: theme.typography.fontWeightBold,
    letterSpacing: "-0.02em",
    color: theme.colors.text.primary,
  }),
  wordLg: css({
    fontSize: theme.typography.h2.fontSize,
    fontWeight: theme.typography.fontWeightBold,
    letterSpacing: "-0.03em",
    color: theme.colors.text.primary,
  }),
  accent: css({ color: theme.colors.text.secondary }),
});
