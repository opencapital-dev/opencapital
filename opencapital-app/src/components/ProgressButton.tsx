import { css, cx, keyframes } from "@emotion/css";
import { GrafanaTheme2 } from "@grafana/data";
import { Button, IconName, useStyles2 } from "@grafana/ui";

type Variant = "primary" | "secondary" | "destructive";
type Size = "sm" | "md" | "lg";
type ButtonFill = "text" | "outline" | "solid";

type Props = {
  /** While true the control IS the progress bar (not a clickable button). */
  active: boolean;
  idleLabel: string;
  activeLabel: string;
  onClick?: () => void;
  disabled?: boolean;
  variant?: Variant;
  size?: Size;
  icon?: IconName;
  /** Idle-button fill (e.g. "outline" for a destructive uninstall). */
  buttonFill?: ButtonFill;
  /** 0–100 for a determinate fill; omit/undefined for an indeterminate sweep. */
  value?: number;
  fullWidth?: boolean;
  className?: string;
};

/**
 * A button that, while an action is in flight, turns into the progress bar for
 * that action — the track and fill live inside the button's own footprint, the
 * label swaps to the active phrase. Determinate when `value` is given, an
 * indeterminate sweep otherwise.
 */
export function ProgressButton({
  active,
  idleLabel,
  activeLabel,
  onClick,
  disabled,
  variant = "primary",
  size = "md",
  icon,
  buttonFill,
  value,
  fullWidth,
  className,
}: Props) {
  const styles = useStyles2(getStyles);

  if (!active) {
    return (
      <Button
        variant={variant}
        size={size}
        icon={icon}
        fill={buttonFill}
        onClick={onClick}
        disabled={disabled}
        fullWidth={fullWidth}
        className={className}
      >
        {idleLabel}
      </Button>
    );
  }

  const determinate = typeof value === "number";
  const pct = determinate ? Math.max(0, Math.min(100, value!)) : 100;

  return (
    <div
      className={cx(styles.bar, styles.track[variant], styles.size[size], fullWidth && styles.full, className)}
      role="progressbar"
      aria-label={activeLabel}
      aria-busy
      aria-valuemin={determinate ? 0 : undefined}
      aria-valuemax={determinate ? 100 : undefined}
      aria-valuenow={determinate ? pct : undefined}
    >
      <span
        className={cx(styles.fill, styles.fillColor[variant], !determinate && styles.indeterminate)}
        style={determinate ? { width: `${pct}%` } : undefined}
      />
      <span className={styles.label}>{activeLabel}</span>
    </div>
  );
}

const sweep = keyframes({
  "0%": { transform: "translateX(-120%)" },
  "100%": { transform: "translateX(320%)" },
});
// One-directional sweep: glide left→right across the fill, then hold off-screen
// for the second half so the loop reset is invisible (no perceived reversal).
const gloss = keyframes({
  "0%": { transform: "translateX(-100%)" },
  "50%": { transform: "translateX(100%)" },
  "100%": { transform: "translateX(100%)" },
});

const getStyles = (theme: GrafanaTheme2) => ({
  bar: css({
    position: "relative",
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    minWidth: 96,
    borderRadius: theme.shape.radius.default,
    fontWeight: theme.typography.fontWeightMedium,
    lineHeight: 1,
    whiteSpace: "nowrap",
    overflow: "hidden",
    userSelect: "none",
    cursor: "default",
  }),
  full: css({ width: "100%" }),
  size: {
    sm: css({ height: 24, padding: theme.spacing(0, 1.5), fontSize: theme.typography.bodySmall.fontSize }),
    md: css({ height: 32, padding: theme.spacing(0, 2), fontSize: theme.typography.body.fontSize }),
    lg: css({ height: 48, padding: theme.spacing(0, 3), fontSize: theme.typography.body.fontSize }),
  },
  track: {
    primary: css({
      color: theme.colors.primary.contrastText,
      background: theme.colors.primary.transparent,
      boxShadow: `inset 0 0 0 1px ${theme.colors.primary.borderTransparent}`,
    }),
    secondary: css({
      color: theme.colors.text.primary,
      background: theme.colors.background.secondary,
      boxShadow: `inset 0 0 0 1px ${theme.colors.border.medium}`,
    }),
    destructive: css({
      color: theme.colors.error.contrastText,
      background: theme.colors.error.transparent,
      boxShadow: `inset 0 0 0 1px ${theme.colors.error.borderTransparent}`,
    }),
  },
  fill: css({
    position: "absolute",
    left: 0,
    top: 0,
    height: "100%",
    borderRadius: "inherit",
    // Clip the gloss to the filled region so the shimmer never spills past the
    // blue into the empty track.
    overflow: "hidden",
    transition: "width 0.45s cubic-bezier(0.22, 1, 0.36, 1)",
    // Moving gloss so a determinate bar still reads as working between updates.
    "&::after": {
      content: '""',
      position: "absolute",
      inset: 0,
      background: "linear-gradient(90deg, transparent, rgba(255, 255, 255, 0.28), transparent)",
      transform: "translateX(-100%)",
      animation: `${gloss} 2s linear infinite`,
    },
    "@media (prefers-reduced-motion: reduce)": {
      transition: "none",
      "&::after": { animation: "none", opacity: 0 },
    },
  }),
  fillColor: {
    primary: css({ background: theme.colors.primary.main }),
    secondary: css({ background: theme.colors.primary.main }),
    destructive: css({ background: theme.colors.error.main }),
  },
  // Indeterminate: a fixed-width segment sweeping across the track.
  indeterminate: css({
    width: "45%",
    animation: `${sweep} 1.2s ease-in-out infinite`,
    "&::after": { display: "none" },
    "@media (prefers-reduced-motion: reduce)": {
      animation: "none",
      width: "100%",
      opacity: 0.5,
    },
  }),
  label: css({
    position: "relative",
    zIndex: 1,
    display: "inline-flex",
    alignItems: "center",
  }),
});
