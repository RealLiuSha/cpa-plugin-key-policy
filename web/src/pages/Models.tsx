import { useEffect, useId, useState } from "react";
import { Link } from "react-router-dom";
import { deleteModelDefinition, fetchModelDefinitions } from "../api/modelDefinitions";
import Modal, { ConfirmDialog } from "../components/Modal";
import ModelImportWizard from "../components/ModelImportWizard";
import ModelPriceImport from "../components/ModelPriceImport";
import type { ModelDefinition } from "../types";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";

function priceLabel(model: ModelDefinition, translate: (key: string, variables?: Record<string, string | number>) => string): string {
  if (model.free) return translate("models.free");
  if (model.billing_mode === "per_call") return translate("models.pricePerCallSummary", { price: model.per_call_usd ?? 0 });
  return translate("models.priceTokenSummary", {
    input: model.input_price_per_million ?? 0,
    output: model.output_price_per_million ?? 0,
    cache: model.cache_read_price_per_million ?? 0,
    write: model.cache_write_price_per_million ?? 0,
  });
}

type Translate = (key: string, variables?: Record<string, string | number>) => string;

function targetLabel(target: ModelDefinition["targets"][number]): string {
  return `${target.provider}${target.group ? ` · ${target.group}` : ""} / ${target.target_model}`;
}

function TargetChips({ model, translate }: { model: ModelDefinition; translate: Translate }) {
  const [expanded, setExpanded] = useState(false);
  const visibleTargets = expanded ? model.targets : model.targets.slice(0, 3);
  const hiddenCount = Math.max(0, model.targets.length - 3);
  return (
    <div className="chip-row">
      {visibleTargets.map((target) => <span className="chip" key={`${target.provider}|${target.group ?? ""}|${target.target_model}`}>{targetLabel(target)}</span>)}
      {hiddenCount > 0 && (
        <button type="button" className="chip more" aria-expanded={expanded} onClick={() => setExpanded((current) => !current)}>
          {expanded ? translate("models.showLessTargets") : translate("models.moreTargets", { count: hiddenCount })}
        </button>
      )}
    </div>
  );
}

function ModelActions({ model, translate, onDelete }: { model: ModelDefinition; translate: Translate; onDelete: (model: ModelDefinition) => void }) {
  const reasonID = useId();
  const referenced = (model.ref_count ?? 0) > 0;
  return (
    <div className="model-actions">
      <div className="card-actions">
        <Link className="btn sm" to={`/models/${encodeURIComponent(model.name)}/edit`}>{translate("models.edit")}</Link>
        <button
          type="button"
          className="btn sm danger"
          disabled={referenced}
          aria-describedby={referenced ? reasonID : undefined}
          onClick={() => onDelete(model)}
        >
          {translate("models.delete")}
        </button>
      </div>
      {referenced && <small id={reasonID} className="action-reason">{translate("models.deleteBlocked", { count: model.ref_count ?? 0 })}</small>}
    </div>
  );
}

export default function Models() {
  const t = useT();
  const [models, setModels] = useState<ModelDefinition[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [showImport, setShowImport] = useState(false);
  const [showSync, setShowSync] = useState(false);
  const [pendingDelete, setPendingDelete] = useState<ModelDefinition | null>(null);
  const [deleting, setDeleting] = useState(false);

  const load = async () => {
    setLoading(true);
    setError("");
    try {
      setModels(await fetchModelDefinitions());
    } catch (reason) {
      setError(extractApiError(reason, t("models.loadFailed")));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => { void load(); }, []);

  const remove = async () => {
    if (!pendingDelete) return;
    const model = pendingDelete;
    setDeleting(true);
    try {
      await deleteModelDefinition(model.name);
      setPendingDelete(null);
      await load();
    } catch (reason) {
      setError(extractApiError(reason, t("models.deleteFailed")));
      setPendingDelete(null);
    } finally {
      setDeleting(false);
    }
  };

  return (
    <div className="page models-page">
      <div className="page-head">
        <div><h1>{t("models.title")}</h1><p className="muted">{t("models.description")}</p></div>
        <div className="page-head-actions">
          <button type="button" className="btn" onClick={() => setShowImport(true)}>{t("models.importFromCpa")}</button>
          <button type="button" className="btn" onClick={() => setShowSync(true)}>{t("models.syncPrices")}</button>
          <Link className="btn primary" to="/models/new">{t("models.new")}</Link>
        </div>
      </div>
      {error && <div className="error">{error}</div>}
      {loading ? <div className="muted">{t("keys.loading")}</div> : models.length === 0 ? (
        <div className="card muted">{t("models.empty")}</div>
      ) : (
        <>
          <div className="card table-wrap model-table mobile-hidden">
            <table>
              <thead><tr><th>{t("models.colName")}</th><th>{t("models.colDispatch")}</th><th>{t("models.colPrice")}</th><th>{t("models.colTargets")}</th><th>{t("models.colReferences")}</th><th>{t("models.colActions")}</th></tr></thead>
              <tbody>{models.map((model) => (
                <tr key={model.name}>
                  <td><strong>{model.name}</strong></td>
                  <td><span className="badge">{model.dispatch === "priority" ? t("models.priority") : t("models.roundRobin")}</span></td>
                  <td className="mono model-price">{priceLabel(model, t)}</td>
                  <td><TargetChips model={model} translate={t} /></td>
                  <td>{model.ref_count ? t("models.refs", { count: model.ref_count }) : t("models.unreferenced")}</td>
                  <td><ModelActions model={model} translate={t} onDelete={setPendingDelete} /></td>
                </tr>
              ))}</tbody>
            </table>
          </div>
          <div className="model-cards mobile-only">
            {models.map((model) => (
              <article className="card model-card" key={model.name}>
                <div className="model-card-head"><h2>{model.name}</h2><span className="badge">{model.dispatch === "priority" ? t("models.priority") : t("models.roundRobin")}</span></div>
                <p className="mono model-price">{priceLabel(model, t)}</p>
                <TargetChips model={model} translate={t} />
                <p className="muted">{model.ref_count ? t("models.refs", { count: model.ref_count }) : t("models.unreferenced")}</p>
                <ModelActions model={model} translate={t} onDelete={setPendingDelete} />
              </article>
            ))}
          </div>
        </>
      )}
      {showImport && (
        <Modal title={t("models.importFromCpaTitle")} closeLabel={t("models.close")} onClose={() => setShowImport(false)} wide>
          <ModelImportWizard onApplied={load} />
        </Modal>
      )}
      {showSync && (
        <Modal title={t("models.syncTitle")} closeLabel={t("models.close")} onClose={() => setShowSync(false)} wide>
          <ModelPriceImport onApplied={load} />
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
