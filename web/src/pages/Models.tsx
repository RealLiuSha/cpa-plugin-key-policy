import { basePriceSummary, modelPriceSummary } from "../components/modelPricing";
import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { deleteModelPrice, fetchModelPrices } from "../api/modelPrices";
import Modal, { ConfirmDialog } from "../components/Modal";
import ModelImport from "../components/ModelImport";
import PriceSync from "../components/PriceSync";
import UnpricedBadge from "../components/UnpricedBadge";
import { modelEditPath, modelNewPath } from "../navigation";
import { formatCount } from "../format";
import type { ModelPriceRow, UnpricedModel } from "../types";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";

type Translate = (key: string, variables?: Record<string, string | number>) => string;

const rowKey = (row: ModelPriceRow) => `${row.name.toLowerCase()}\u0000${row.provider.toLowerCase()}`;

function providerLabel(row: ModelPriceRow, t: Translate): string {
  return row.provider || t("models.anyProvider");
}

function ModelActions({ row, translate, onDelete }: { row: ModelPriceRow; translate: Translate; onDelete: (row: ModelPriceRow) => void }) {
  return (
    <div className="card-actions">
      <Link className="btn sm" to={modelEditPath(row)}>{translate("models.edit")}</Link>
      <button type="button" className="btn sm danger-outline" onClick={() => onDelete(row)}>{translate("models.delete")}</button>
    </div>
  );
}

export default function Models() {
  const t = useT();
  const [rows, setRows] = useState<ModelPriceRow[]>([]);
  const [unpriced, setUnpriced] = useState<UnpricedModel[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [showImport, setShowImport] = useState(false);
  // The price sync to show: model ids to tick, and whether they are unpriced
  // models that each need a row pricing their calls.
  const [syncFor, setSyncFor] = useState<{ names: string[]; unpriced: boolean } | null>(null);
  const [pendingDelete, setPendingDelete] = useState<ModelPriceRow | null>(null);
  const [deleting, setDeleting] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      const prices = await fetchModelPrices();
      setRows(prices.models);
      setUnpriced(prices.unpriced);
    } catch (reason) {
      setError(extractApiError(reason, t("models.loadFailed")));
    } finally {
      setLoading(false);
    }
  }, [t]);

  useEffect(() => { void load(); }, [load]);

  const remove = async () => {
    if (!pendingDelete) return;
    setDeleting(true);
    try {
      await deleteModelPrice(pendingDelete);
      setPendingDelete(null);
      await load();
    } catch (reason) {
      setError(extractApiError(reason, t("models.deleteFailed")));
      setPendingDelete(null);
    } finally {
      setDeleting(false);
    }
  };

  const billingLabel = (row: ModelPriceRow) => row.billing_mode === "per_call" ? t("models.perCall") : t("models.tokens");
  const multiplierLabel = (row: ModelPriceRow) => row.billing_mode === "tokens" ? `×${row.billing_multiplier ?? 1}` : "—";
  const basePrice = (row: ModelPriceRow) => row.priced ? basePriceSummary(row, t) : <UnpricedBadge />;
  const chargedPrice = (row: ModelPriceRow) => row.priced ? modelPriceSummary(row, t) : "—";

  return (
    <div className="page models-page">
      <div className="page-head">
        <div><h1>{t("models.title")}</h1><p className="muted">{t("models.description")}</p></div>
        <div className="page-head-actions">
          <button type="button" className="btn" onClick={() => setShowImport(true)}>{t("models.importFromCpa")}</button>
          <button type="button" className="btn" onClick={() => setSyncFor({ names: [], unpriced: false })}>{t("models.syncPrices")}</button>
          <Link className="btn primary" to={modelNewPath()}>{t("models.new")}</Link>
        </div>
      </div>
      {error && <div className="error" role="alert">{error}</div>}
      {unpriced.length > 0 && (
        <section className="notice unpriced-notice" role="status" aria-label={t("models.unpricedNotice", { count: unpriced.length })}>
          <div className="notice-row">
            <span>{t("models.unpricedNotice", { count: unpriced.length })}</span>
            <button type="button" className="btn sm" onClick={() => setSyncFor({ names: unpriced.map((model) => model.name), unpriced: true })}>{t("models.priceUnpriced")}</button>
          </div>
          <ul className="unpriced-list">
            {unpriced.map((model) => (
              <li key={model.name}>
                <span className="mono">{model.name}</span>
                <span className="muted">{t("models.unpricedUsage", { calls: formatCount(model.calls), date: model.last_seen })}</span>
                <Link className="btn sm" to={modelNewPath(model.name)}>{t("models.addManually")}</Link>
                {model.has_qualified_rows && <small className="unpriced-qualified">{t("usage.unpricedQualified")}</small>}
              </li>
            ))}
          </ul>
        </section>
      )}
      {loading ? <div className="muted">{t("keys.loading")}</div> : rows.length === 0 ? (
        <div className="card muted">{t("models.empty")}</div>
      ) : (
        <>
          <div className="card table-wrap model-table">
            <table>
              <thead><tr>
                <th>{t("models.colName")}</th><th>{t("models.colProvider")}</th><th>{t("models.colBilling")}</th>
                <th>{t("models.colBasePrice")}</th><th>{t("models.colMultiplier")}</th><th>{t("models.colChargedPrice")}</th><th>{t("models.colActions")}</th>
              </tr></thead>
              <tbody>{rows.map((row) => (
                <tr key={rowKey(row)}>
                  <td className="model-name"><strong className="mono">{row.name}</strong></td>
                  <td className={row.provider ? "mono" : "muted"}>{providerLabel(row, t)}</td>
                  <td><span className="badge">{billingLabel(row)}</span></td>
                  <td className="model-price">{basePrice(row)}</td>
                  <td className="mono">{multiplierLabel(row)}</td>
                  <td className="model-price">{chargedPrice(row)}</td>
                  <td><ModelActions row={row} translate={t} onDelete={setPendingDelete} /></td>
                </tr>
              ))}</tbody>
            </table>
          </div>
          <div className="model-cards mobile-only">
            {rows.map((row) => (
              <article className="card model-card" key={rowKey(row)}>
                <div className="model-card-head"><h2 className="mono">{row.name}</h2><span className="badge">{billingLabel(row)}</span></div>
                <p className="muted">{t("models.colProvider")}：{providerLabel(row, t)} · {multiplierLabel(row)}</p>
                <div className="model-price">{basePrice(row)}</div>
                {row.priced && row.billing_mode === "tokens" && (row.billing_multiplier ?? 1) !== 1 && <small className="muted">{t("models.chargedPrices")}：{chargedPrice(row)}</small>}
                <ModelActions row={row} translate={t} onDelete={setPendingDelete} />
              </article>
            ))}
          </div>
        </>
      )}
      {showImport && (
        <Modal title={t("models.importFromCpaTitle")} closeLabel={t("models.close")} onClose={() => setShowImport(false)} wide>
          <ModelImport
            existing={rows}
            onImported={load}
            onSyncPrices={(names) => { setShowImport(false); setSyncFor({ names, unpriced: false }); }}
            onDone={() => setShowImport(false)}
          />
        </Modal>
      )}
      {syncFor && (
        <Modal title={t("models.syncTitle")} closeLabel={t("models.close")} onClose={() => setSyncFor(null)} wide>
          <PriceSync preselect={syncFor.names} unpriced={syncFor.unpriced} onApplied={load} onDone={() => setSyncFor(null)} />
        </Modal>
      )}
      {pendingDelete && (
        <ConfirmDialog
          title={t("models.deleteTitle")}
          message={t("models.deleteConfirm", { name: pendingDelete.name, provider: providerLabel(pendingDelete, t) })}
          cancelLabel={t("keyForm.cancel")}
          confirmLabel={t("models.confirmDelete")}
          busy={deleting}
          onCancel={() => setPendingDelete(null)}
          onConfirm={() => void remove()}
        />
      )}
    </div>
  );
}
