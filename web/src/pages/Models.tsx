import { basePriceSummary, isUnpriced, modelPriceSummary } from "../components/modelPricing";
import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { deleteModelDefinition, fetchModelDefinitions } from "../api/modelDefinitions";
import Modal, { ConfirmDialog } from "../components/Modal";
import ModelImport from "../components/ModelImport";
import PriceSync from "../components/PriceSync";
import type { ModelDefinition } from "../types";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";

type Translate = (key: string, variables?: Record<string, string | number>) => string;

function PriceCell({ model, translate }: { model: ModelDefinition; translate: Translate }) {
  if (isUnpriced(model)) return <span className="badge warn">{translate("models.unpriced")}</span>;
  const multiplier = model.billing_multiplier ?? 1;
  return (
    <div className="model-price">
      <div>{basePriceSummary(model, translate)}</div>
      {model.billing_mode === "tokens" && multiplier !== 1 && <small className="muted">{translate("models.chargedPrices")}：{modelPriceSummary(model, translate)}</small>}
    </div>
  );
}

function ModelActions({ model, translate, onDelete }: { model: ModelDefinition; translate: Translate; onDelete: (model: ModelDefinition) => void }) {
  return (
    <div className="card-actions">
      <Link className="btn sm" to={`/models/${encodeURIComponent(model.name)}/edit`}>{translate("models.edit")}</Link>
      {!model.ref_count && <button type="button" className="btn sm danger-outline" onClick={() => onDelete(model)}>{translate("models.delete")}</button>}
    </div>
  );
}

export default function Models() {
  const t = useT();
  const [models, setModels] = useState<ModelDefinition[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [showImport, setShowImport] = useState(false);
  const [syncFor, setSyncFor] = useState<string[] | null>(null);
  const [pendingDelete, setPendingDelete] = useState<ModelDefinition | null>(null);
  const [deleting, setDeleting] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      setModels(await fetchModelDefinitions());
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
      await deleteModelDefinition(pendingDelete.name);
      setPendingDelete(null);
      await load();
    } catch (reason) {
      setError(extractApiError(reason, t("models.deleteFailed")));
      setPendingDelete(null);
    } finally {
      setDeleting(false);
    }
  };

  const unpriced = models.filter((model) => model.billing_mode === "tokens" && isUnpriced(model));
  const billingLabel = (model: ModelDefinition) => model.billing_mode === "per_call" ? t("models.perCall") : t("models.tokens");
  const multiplierLabel = (model: ModelDefinition) => model.billing_mode === "tokens" ? `×${model.billing_multiplier ?? 1}` : "—";
  const refsLabel = (model: ModelDefinition) => model.ref_count ? t("models.refs", { count: model.ref_count }) : t("models.unreferenced");

  return (
    <div className="page models-page">
      <div className="page-head">
        <div><h1>{t("models.title")}</h1><p className="muted">{t("models.description")}</p></div>
        <div className="page-head-actions">
          <button type="button" className="btn" onClick={() => setShowImport(true)}>{t("models.importFromCpa")}</button>
          <button type="button" className="btn" onClick={() => setSyncFor([])}>{t("models.syncPrices")}</button>
          <Link className="btn primary" to="/models/new">{t("models.new")}</Link>
        </div>
      </div>
      {error && <div className="error" role="alert">{error}</div>}
      {unpriced.length > 0 && (
        <div className="notice notice-row" role="status">
          <span>{t("models.unpricedNotice", { count: unpriced.length, names: unpriced.map((model) => model.name).join("、") })}</span>
          <button type="button" className="btn sm" onClick={() => setSyncFor(unpriced.map((model) => model.name))}>{t("models.syncPrices")}</button>
        </div>
      )}
      {loading ? <div className="muted">{t("keys.loading")}</div> : models.length === 0 ? (
        <div className="card muted">{t("models.empty")}</div>
      ) : (
        <>
          <div className="card table-wrap model-table">
            <table>
              <thead><tr>
                <th>{t("models.colName")}</th><th>{t("models.colUpstream")}</th><th>{t("models.colBilling")}</th>
                <th>{t("models.colPrice")}</th><th>{t("models.colMultiplier")}</th><th>{t("models.colReferences")}</th><th>{t("models.colActions")}</th>
              </tr></thead>
              <tbody>{models.map((model) => (
                <tr key={model.name}>
                  <td className="model-name"><strong>{model.name}</strong></td>
                  <td><span className="mono">{model.provider} / {model.target_model}</span></td>
                  <td><span className="badge">{billingLabel(model)}</span></td>
                  <td><PriceCell model={model} translate={t} /></td>
                  <td className="mono">{multiplierLabel(model)}</td>
                  <td>{refsLabel(model)}</td>
                  <td><ModelActions model={model} translate={t} onDelete={setPendingDelete} /></td>
                </tr>
              ))}</tbody>
            </table>
          </div>
          <div className="model-cards mobile-only">
            {models.map((model) => (
              <article className="card model-card" key={model.name}>
                <div className="model-card-head"><h2>{model.name}</h2><span className="badge">{billingLabel(model)}</span></div>
                <p className="mono muted">{model.provider} / {model.target_model} · {multiplierLabel(model)}</p>
                <PriceCell model={model} translate={t} />
                <p className="muted">{refsLabel(model)}</p>
                <ModelActions model={model} translate={t} onDelete={setPendingDelete} />
              </article>
            ))}
          </div>
        </>
      )}
      {showImport && (
        <Modal title={t("models.importFromCpaTitle")} closeLabel={t("models.close")} onClose={() => setShowImport(false)} wide>
          <ModelImport
            existing={models}
            onImported={load}
            onSyncPrices={(names) => { setShowImport(false); setSyncFor(names); }}
            onDone={() => setShowImport(false)}
          />
        </Modal>
      )}
      {syncFor && (
        <Modal title={t("models.syncTitle")} closeLabel={t("models.close")} onClose={() => setSyncFor(null)} wide>
          <PriceSync preselect={syncFor} onApplied={load} onDone={() => setSyncFor(null)} />
        </Modal>
      )}
      {pendingDelete && (
        <ConfirmDialog
          title={t("models.deleteTitle")}
          message={t("models.deleteConfirm", { name: pendingDelete.name })}
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
