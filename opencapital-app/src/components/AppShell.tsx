import { ReactNode } from "react";
import { css } from "@emotion/css";
import { GrafanaTheme2 } from "@grafana/data";
import { Dropdown, Icon, IconName, Menu, Text, useStyles2 } from "@grafana/ui";
import { Brand } from "./Brand";
import { OrgSwitcher } from "./OrgSwitcher";
import type { Org } from "../types";

export type NavKey = "launch" | "plugins" | "settings";

type Props = {
  displayName: string;
  orgs: Org[];
  selectedOrg: Org | null;
  nav: NavKey;
  busy: boolean;
  onNav: (key: NavKey) => void;
  onSelectOrg: (org: Org) => void;
  onCreateOrg: () => void;
  onLogout: () => void;
  children: ReactNode;
};

const NAV: Array<{ key: NavKey; label: string; icon: IconName }> = [
  { key: "launch", label: "Launch", icon: "rocket" },
  { key: "plugins", label: "Plugins", icon: "apps" },
  { key: "settings", label: "Settings", icon: "cog" },
];

export function AppShell({
  displayName,
  orgs,
  selectedOrg,
  nav,
  busy,
  onNav,
  onSelectOrg,
  onCreateOrg,
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
          <OrgSwitcher
            orgs={orgs}
            selected={selectedOrg}
            disabled={busy}
            onSelect={onSelectOrg}
            onCreate={onCreateOrg}
          />
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
          {selectedOrg ? children : <NoOrg onCreate={onCreateOrg} />}
        </main>
      </div>
    </div>
  );
}

function NoOrg({ onCreate }: { onCreate: () => void }) {
  const styles = useStyles2(getStyles);
  return (
    <div className={styles.noOrg}>
      <Icon name="folder-open" size="xxl" />
      <Text element="h2" variant="h4">
        No workspace selected
      </Text>
      <Text color="secondary">Pick a workspace above, or create your first one.</Text>
      <button className={styles.linkBtn} onClick={onCreate} type="button">
        <Icon name="plus" /> New workspace
      </button>
    </div>
  );
}

const getStyles = (theme: GrafanaTheme2) => ({
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
    height: 52,
    padding: theme.spacing(0, 2),
    background: theme.colors.background.primary,
    borderBottom: `1px solid ${theme.colors.border.weak}`,
    flexShrink: 0,
  }),
  topRight: css({
    display: "flex",
    alignItems: "center",
    gap: theme.spacing(3),
  }),
  identity: css({
    display: "flex",
    alignItems: "center",
    gap: theme.spacing(1),
    maxWidth: 260,
    overflow: "hidden",
    padding: theme.spacing(0.5, 1),
    border: "none",
    borderRadius: theme.shape.radius.default,
    background: "transparent",
    color: theme.colors.text.secondary,
    cursor: "pointer",
    "&:hover": { background: theme.colors.action.hover },
  }),
  caret: css({ color: theme.colors.text.secondary, flexShrink: 0 }),
  avatar: css({
    width: 26,
    height: 26,
    flexShrink: 0,
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    borderRadius: theme.shape.radius.circle,
    background: theme.colors.primary.main,
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
    width: 92,
    flexShrink: 0,
    display: "flex",
    flexDirection: "column",
    gap: theme.spacing(0.5),
    padding: theme.spacing(1.5, 1),
    background: theme.colors.background.primary,
    borderRight: `1px solid ${theme.colors.border.weak}`,
  }),
  navItem: css({
    display: "flex",
    flexDirection: "column",
    alignItems: "center",
    gap: theme.spacing(0.5),
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
    "&[data-active='true']": {
      background: theme.colors.action.selected,
      color: theme.colors.text.maxContrast,
      boxShadow: `inset 2px 0 0 ${theme.colors.primary.main}`,
    },
  }),
  content: css({
    flex: 1,
    minWidth: 0,
    overflowY: "auto",
    overflowX: "hidden",
    padding: theme.spacing(4),
  }),
  noOrg: css({
    height: "100%",
    display: "flex",
    flexDirection: "column",
    alignItems: "center",
    justifyContent: "center",
    gap: theme.spacing(1.5),
    color: theme.colors.text.secondary,
    textAlign: "center",
  }),
  linkBtn: css({
    display: "inline-flex",
    alignItems: "center",
    gap: theme.spacing(1),
    marginTop: theme.spacing(1),
    padding: theme.spacing(1, 2),
    border: `1px solid ${theme.colors.border.medium}`,
    borderRadius: theme.shape.radius.default,
    background: "transparent",
    color: theme.colors.text.primary,
    cursor: "pointer",
    fontSize: theme.typography.body.fontSize,
    "&:hover": { background: theme.colors.action.hover },
  }),
});
