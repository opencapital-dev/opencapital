import { css } from "@emotion/css";
import { GrafanaTheme2 } from "@grafana/data";
import { useStyles2 } from "@grafana/ui";

type Props = {
  size?: "sm" | "lg";
};

/** OpenCapital wordmark + a small candlestick glyph drawn in brand blue. */
export function Brand({ size = "sm" }: Props) {
  const styles = useStyles2(getStyles);
  const px = size === "lg" ? 34 : 24;

  return (
    <div className={styles.wrap}>
      <span className={styles.glyph} style={{ width: px, height: px }}>
        <svg width={px} height={px} viewBox="0 0 24 24" fill="none" aria-hidden>
          <rect x="4" y="9" width="3" height="9" rx="1" className={styles.barLow} />
          <rect x="4" y="6" width="3" height="3" rx="1" className={styles.wick} />
          <rect x="10.5" y="4" width="3" height="11" rx="1" className={styles.barHigh} />
          <rect x="10.5" y="15" width="3" height="3" rx="1" className={styles.wick} />
          <rect x="17" y="11" width="3" height="7" rx="1" className={styles.barLow} />
          <rect x="17" y="8" width="3" height="3" rx="1" className={styles.wick} />
        </svg>
      </span>
      <span className={size === "lg" ? styles.wordLg : styles.word}>
        Open<span className={styles.accent}>Capital</span>
      </span>
    </div>
  );
}

const getStyles = (theme: GrafanaTheme2) => ({
  wrap: css({
    display: "inline-flex",
    alignItems: "center",
    gap: theme.spacing(1.25),
    userSelect: "none",
  }),
  // A soft brand-tinted disc behind the glyph: turns the loose bars into one
  // deliberate mark and gives the wordmark an anchor.
  glyph: css({
    flexShrink: 0,
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    borderRadius: theme.shape.radius.default,
    background: theme.colors.primary.transparent,
    boxShadow: `inset 0 0 0 1px ${theme.colors.primary.borderTransparent}`,
  }),
  barHigh: css({ fill: theme.colors.primary.text }),
  barLow: css({ fill: theme.colors.primary.main }),
  wick: css({ fill: theme.colors.primary.text, opacity: 0.45 }),
  word: css({
    fontSize: theme.typography.h5.fontSize,
    fontWeight: theme.typography.fontWeightBold,
    letterSpacing: "-0.02em",
    color: theme.colors.text.primary,
    lineHeight: 1,
  }),
  wordLg: css({
    fontSize: theme.typography.h2.fontSize,
    fontWeight: theme.typography.fontWeightBold,
    letterSpacing: "-0.03em",
    color: theme.colors.text.primary,
    lineHeight: 1,
  }),
  // Second half in brand blue — ties the wordmark to the glyph without a
  // gradient. primary.text holds AA contrast on the dark chrome.
  accent: css({ color: theme.colors.primary.text }),
});
