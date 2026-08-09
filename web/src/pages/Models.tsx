import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { deleteModelDefinition, fetchModelDefinitions } from "../api/modelDefinitions";
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
  });
}

export default function Models() {
  const t = useT();
  const [models, setModels] = useState<ModelDefinition[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

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

  const remove = async (model: ModelDefinition) => {
    if (!confirm(t("models.deleteConfirm", { name: model.name }))) return;
    try {
      await deleteModelDefinition(model.name);
      await load();
    } catch (reason) {
      setError(extractApiError(reason, t("models.deleteFailed")));
    }
  };

  return (
    <div className="page models-page">
      <div className="page-head">
        <div><h1>{t("models.title")}</h1><p className="muted">{t("models.description")}</p></div>
        <Link className="btn primary" to="/models/new">{t("models.new")}</Link>
      </div>
      {error && <div className="error">{error}</div>}
      {loading ? <div className="muted">{t("keys.loading")}</div> : models.length === 0 ? (
        <div className="card muted">{t("models.empty")}</div>
      ) : (
        <div className="model-cards">
          {models.map((model) => (
            <article className="card model-card" key={model.name}>
              <div className="model-card-head"><h2>{model.name}</h2><span className="badge">{model.dispatch === "priority" ? t("models.priority") : t("models.roundRobin")}</span></div>
              <p>{priceLabel(model, t)}</p>
              <div className="chip-row">{model.targets.map((target) => <span className="chip" key={`${target.provider}|${target.group ?? ""}|${target.target_model}`}>{target.provider}{target.group ? ` · ${target.group}` : ""} / {target.target_model}</span>)}</div>
              <p className="muted">{model.ref_count ? t("models.refs", { count: model.ref_count }) : t("models.unreferenced")}</p>
              <div className="card-actions">
                <Link className="btn sm" to={`/models/${encodeURIComponent(model.name)}/edit`}>{t("models.edit")}</Link>
                <button className="btn sm danger" disabled={(model.ref_count ?? 0) > 0} onClick={() => void remove(model)}>{t("models.delete")}</button>
              </div>
            </article>
          ))}
        </div>
      )}
      <ModelPriceImport onApplied={load} onError={setError} />
    </div>
  );
}
