import { useEffect, useMemo, useState } from "react";
import { fetchCatalog } from "../api/models";
import { extractApiError } from "../api/error";
import Modal from "./Modal";
import type { CatalogModel } from "../types";
import { useT } from "../i18n";

interface Props {
  current?: { provider: string; target_model: string };
  onPick: (choice: CatalogModel) => void;
  onClose: () => void;
}

// Lists the capabilities CPA can serve right now and returns one choice.
export default function UpstreamPicker({ current, onPick, onClose }: Props) {
  const t = useT();
  const [catalog, setCatalog] = useState<CatalogModel[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [query, setQuery] = useState("");

  useEffect(() => {
    let alive = true;
    void fetchCatalog(current?.provider ? new Set([current.provider]) : undefined)
      .then((rows) => { if (alive) setCatalog(rows); })
      .catch((reason) => { if (alive) setError(extractApiError(reason, t("picker.loadFailed"))); })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, [current?.provider, t]);

  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase();
    return needle ? catalog.filter((row) => row.model.toLowerCase().includes(needle) || row.provider.includes(needle)) : catalog;
  }, [catalog, query]);

  const isCurrent = (row: CatalogModel) =>
    row.provider === current?.provider?.toLowerCase() && row.model === current?.target_model;

  return (
    <Modal title={t("picker.targetTitle")} closeLabel={t("models.close")} onClose={onClose} wide>
      <p className="muted">{t("picker.targetHint")}</p>
      {loading ? <div className="muted">{t("picker.loading")}</div> : error ? <div className="error">{error}</div> : catalog.length === 0 ? (
        <div className="muted">{t("picker.empty")}</div>
      ) : (
        <>
          <input className="input" autoFocus aria-label={t("picker.searchPlaceholder")} placeholder={t("picker.searchPlaceholder")} value={query} onChange={(event) => setQuery(event.target.value)} />
          <div className="choice-list" role="listbox" aria-label={t("picker.targetTitle")}>
            {visible.length === 0 ? <div className="empty-state">{t("picker.noMatch")}</div> : visible.map((row) => (
              <button
                type="button"
                role="option"
                aria-selected={isCurrent(row)}
                className={"choice-row" + (isCurrent(row) ? " active" : "")}
                key={`${row.provider}|${row.model}`}
                onClick={() => onPick(row)}
              >
                <span className="mono">{row.model}</span>
                <span className="badge">{row.provider}</span>
              </button>
            ))}
          </div>
        </>
      )}
    </Modal>
  );
}
