import { css } from "@emotion/css";
import { GrafanaTheme2, SelectableValue } from "@grafana/data";
import { Icon, Select, useStyles2 } from "@grafana/ui";
import type { Org } from "../types";

type Props = {
  orgs: Org[];
  selected: Org | null;
  disabled?: boolean;
  onSelect: (org: Org) => void;
  onCreate: () => void;
};

const NEW = "__new__";

export function OrgSwitcher({ orgs, selected, disabled, onSelect, onCreate }: Props) {
  const styles = useStyles2(getStyles);

  const options: Array<SelectableValue<string>> = orgs.map((o) => ({
    label: o.name,
    value: o.org_id,
    description: `${o.base_currency} · ${o.role}`,
    imgUrl: undefined,
  }));
  options.push({ label: "New workspace…", value: NEW, icon: "plus" });

  return (
    <div className={styles.wrap}>
      <Icon name="folder" className={styles.lead} />
      <Select<string>
        aria-label="Workspace"
        width={30}
        options={options}
        value={selected?.org_id ?? null}
        placeholder="Select workspace"
        disabled={disabled}
        onChange={(v) => {
          if (v.value === NEW) {
            onCreate();
            return;
          }
          const org = orgs.find((o) => o.org_id === v.value);
          if (org) onSelect(org);
        }}
      />
    </div>
  );
}

const getStyles = (theme: GrafanaTheme2) => ({
  wrap: css({
    display: "flex",
    alignItems: "center",
    gap: theme.spacing(1),
  }),
  lead: css({ color: theme.colors.text.secondary }),
});
