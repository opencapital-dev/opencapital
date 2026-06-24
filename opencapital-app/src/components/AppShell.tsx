import { ReactNode } from "react";
import { css } from "@emotion/css";
import { GrafanaTheme2 } from "@grafana/data";
import { Dropdown, Icon, IconName, Menu, Text, useStyles2 } from "@grafana/ui";
import { Brand } from "./Brand";

export type NavKey = "launch" | "plugins" | "sources" | "settings";

type Props = {
  displayName: string;
  nav: NavKey;
  busy: boolean;
  onNav: (key: NavKey) => void;
  onLogout: () => void;
  children: ReactNode;
};

const NAV: Array<{ key: NavKey; label: string; icon: IconName }> = [
  { key: "launch", label: "Launch", icon: "rocket" },
  { key: "plugins", label: "Plugins", icon: "apps" },
  { key: "sources", label: "Sources", icon: "link" },
  { key: "settings", label: "Settings", icon: "cog" },
];

export function AppShell({
  displayName,
  nav,
  busy: _busy,
  onNav,
  onLogout,
  children,
}: Props) {
  const styles = useStyles2(getStyles);

  const userMenu = (
    <Menu>
      <Menu.Item label={displayName} icon="user" disabled />
      <Menu.Divider />
      <Menu.Item label="Log out" icon="signout" destructive onClick={onLogout} />
    </Menu>
  );

  return (
    <div className={styles.shell}>
      <header className={styles.topbar}>
        <Brand />
        <div className={styles.topRight}>
          <Dropdown overlay={userMenu} placement="bottom-end">
            <button className={styles.identity} type="button" title={displayName}>
              <span className={styles.avatar}>
                {displayName.slice(0, 1).toUpperCase()}
              </span>
              <Text variant="bodySmall" color="secondary">
                {displayName}
              </Text>
              <Icon name="angle-down" className={styles.caret} />
            </button>
          </Dropdown>
        </div>
      </header>

      <div className={styles.body}>
        <nav className={styles.rail}>
          {NAV.map((item) => (
            <button
              key={item.key}
              className={styles.navItem}
              data-active={nav === item.key}
              onClick={() => onNav(item.key)}
              type="button"
            >
              <Icon name={item.icon} size="lg" />
              <span>{item.label}</span>
            </button>
          ))}
        </nav>

        <main className={styles.content}>
          {children}
        </main>
      </div>
    </div>
  );
}

const getStyles = (theme: GrafanaTheme2) => {
  // Shared focus ring for the shell's bespoke (non-@grafana/ui) controls, so
  // keyboard focus is as legible as on the library components.
  const focusRing = {
    outline: "none",
    boxShadow: `0 0 0 2px ${theme.colors.background.primary}, 0 0 0 4px ${theme.colors.primary.main}`,
  };
  return {
    shell: css({
      display: "flex",
      flexDirection: "column",
      height: "100vh",
      background: theme.colors.background.canvas,
      color: theme.colors.text.primary,
    }),
    topbar: css({
      display: "flex",
      alignItems: "center",
      justifyContent: "space-between",
      gap: theme.spacing(2),
      height: 56,
      padding: theme.spacing(0, 2, 0, 2.5),
      background: theme.colors.background.primary,
      borderBottom: `1px solid ${theme.colors.border.weak}`,
      // A hair of elevation so the chrome reads as a layer above the canvas —
      // Grafana's canvas/primary are nearly the same value on their own.
      boxShadow: theme.shadows.z1,
      flexShrink: 0,
      position: "relative",
      zIndex: 2,
    }),
    topRight: css({
      display: "flex",
      alignItems: "center",
      gap: theme.spacing(2),
    }),
    identity: css({
      display: "flex",
      alignItems: "center",
      gap: theme.spacing(1),
      maxWidth: 260,
      overflow: "hidden",
      padding: theme.spacing(0.5, 1, 0.5, 0.5),
      border: "none",
      borderRadius: theme.shape.radius.pill,
      background: "transparent",
      color: theme.colors.text.secondary,
      cursor: "pointer",
      transition: "background 0.15s ease",
      "&:hover": { background: theme.colors.action.hover },
      "&:focus-visible": focusRing,
    }),
    caret: css({ color: theme.colors.text.secondary, flexShrink: 0 }),
    avatar: css({
      width: 28,
      height: 28,
      flexShrink: 0,
      display: "inline-flex",
      alignItems: "center",
      justifyContent: "center",
      borderRadius: theme.shape.radius.circle,
      // On-brand blue (Grafana's gradients.brandHorizontal is orange, which
      // would fight OpenCapital's blue); shade->main gives subtle depth.
      background: `linear-gradient(135deg, ${theme.colors.primary.main}, ${theme.colors.primary.shade})`,
      color: theme.colors.primary.contrastText,
      fontSize: theme.typography.bodySmall.fontSize,
      fontWeight: theme.typography.fontWeightBold,
    }),
    body: css({
      display: "flex",
      flex: 1,
      minHeight: 0,
    }),
    rail: css({
      width: 88,
      flexShrink: 0,
      display: "flex",
      flexDirection: "column",
      gap: theme.spacing(0.75),
      padding: theme.spacing(2, 1.5),
      background: theme.colors.background.primary,
      borderRight: `1px solid ${theme.colors.border.weak}`,
    }),
    navItem: css({
      position: "relative",
      display: "flex",
      flexDirection: "column",
      alignItems: "center",
      gap: theme.spacing(0.75),
      padding: theme.spacing(1.5, 0.5),
      border: "none",
      borderRadius: theme.shape.radius.default,
      background: "transparent",
      color: theme.colors.text.secondary,
      cursor: "pointer",
      fontSize: theme.typography.bodySmall.fontSize,
      fontWeight: theme.typography.fontWeightMedium,
      transition: "background 0.15s ease, color 0.15s ease",
      "&:hover": {
        background: theme.colors.action.hover,
        color: theme.colors.text.primary,
      },
      "&:focus-visible": focusRing,
      // Selected: a brand-tinted fill with brand-colored content. Reads clearly
      // as active without a colored side-stripe.
      "&[data-active='true']": {
        background: theme.colors.primary.transparent,
        color: theme.colors.primary.text,
        boxShadow: `inset 0 0 0 1px ${theme.colors.primary.borderTransparent}`,
      },
    }),
    content: css({
      flex: 1,
      minWidth: 0,
      overflowY: "auto",
      overflowX: "hidden",
      padding: theme.spacing(4),
    }),
  };
};
