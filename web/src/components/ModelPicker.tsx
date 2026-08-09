import { useEffect, useMemo, useState } from "react";
import { fetchCatalog, formatTierLabel, groupByCatalog, type CatalogGroup } from "../api/models";
import type { ModelTarget } from "../types";
import { useT } from "../i18n";

interface Props {
  initial?: ModelTarget[];
  onChange: (targets: ModelTarget[]) => void;
}

function targetKey(provider: string, group: string | undefined, model: string): string {
  return `${provider.toLowerCase()}|${(group ?? "").toLowerCase()}|${model.toLowerCase()}`;
}

export default function ModelPicker({ initial = [], onChange }: Props) {
  const t = useT();
  const [groups, setGroups] = useState<CatalogGroup[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState(() => new Set(initial.map((target) => targetKey(target.provider, target.group, target.target_model))));

  useEffect(() => {
    let alive = true;
    const providers = new Set(initial.map((target) => target.provider.toLowerCase()));
    void fetchCatalog(providers)
      .then((catalog) => {
        if (alive) setGroups(groupByCatalog(catalog));
      })
      .catch((reason) => {
        if (alive) setError((reason as Error).message || t("picker.loadFailed"));
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => { alive = false; };
    // The picker owns one form session; parent remounts it for a different model.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (groups.length === 0) return;
    const covered = new Set<string>();
    const targets: ModelTarget[] = [];
    for (const group of groups) {
      for (const model of group.models) {
        const key = targetKey(group.provider, group.group, model);
        covered.add(key);
        if (selected.has(key)) {
          targets.push({ provider: group.provider, target_model: model, ...(group.group ? { group: group.group } : {}) });
        }
      }
    }
    for (const key of selected) {
      if (covered.has(key)) continue;
      const [provider, group, ...modelParts] = key.split("|");
      const model = modelParts.join("|");
      if (provider && model) targets.push({ provider, target_model: model, ...(group ? { group } : {}) });
    }
    onChange(targets);
  }, [groups, onChange, selected]);

  const filtered = useMemo(() => {
    const needle = query.trim().toLowerCase();
    if (!needle) return groups;
    return groups.map((group) => ({
      ...group,
      models: group.models.filter((model) =>
        model.toLowerCase().includes(needle) || group.provider.includes(needle) || (group.group ?? "").includes(needle),
      ),
    })).filter((group) => group.models.length > 0);
  }, [groups, query]);

  const toggle = (group: CatalogGroup, model: string) => {
    const key = targetKey(group.provider, group.group, model);
    setSelected((previous) => {
      const next = new Set(previous);
      if (next.has(key)) next.delete(key); else next.add(key);
      return next;
    });
  };

  if (loading) return <div className="muted">{t("picker.loading")}</div>;
  if (error) return <div className="error">{error}</div>;
  if (groups.length === 0) return <div className="muted">{t("picker.empty")}</div>;

  return (
    <div>
      <input className="input" placeholder={t("picker.searchPlaceholder")} value={query} onChange={(event) => setQuery(event.target.value)} />
      <div className="muted">{t("picker.selectedTargets", { count: selected.size })}</div>
      {filtered.map((group) => {
        const label = group.group ? formatTierLabel(t, group.group) : "";
        return (
          <div className="picker-group" key={`${group.provider}|${group.group ?? ""}`}>
            <div className="pg-head"><span>{group.provider}{label ? ` · ${label}` : ""}</span></div>
            <div className="pg-models">
              {group.models.map((model) => {
                const key = targetKey(group.provider, group.group, model);
                return (
                  <label key={key} className={selected.has(key) ? "active" : ""}>
                    <input type="checkbox" checked={selected.has(key)} onChange={() => toggle(group, model)} />
                    {model}
                  </label>
                );
              })}
            </div>
          </div>
        );
      })}
    </div>
  );
}
