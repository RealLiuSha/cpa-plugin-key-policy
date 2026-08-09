import { useState } from "react";
import { importModelPrices } from "../api/modelDefinitions";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";
import type { PriceImportResult } from "../types";

interface ModelPriceImportProps {
  onApplied: () => Promise<void>;
}

export default function ModelPriceImport({ onApplied }: ModelPriceImportProps) {
  const t = useT();
  const [importText, setImportText] = useState("");
  const [importResult, setImportResult] = useState<PriceImportResult | null>(null);
  const [error, setError] = useState("");

  const runImport = async (dryRun: boolean) => {
    setError("");
    try {
      const parsed = JSON.parse(importText) as { matches?: unknown } | unknown[];
      const matches = Array.isArray(parsed) ? parsed : parsed.matches;
      if (!Array.isArray(matches)) throw new Error(t("models.importInvalid"));
      const result = await importModelPrices({ dry_run: dryRun, matches });
      setImportResult(result);
      if (!dryRun) await onApplied();
    } catch (reason) {
      setError(extractApiError(reason, t("models.importInvalid")));
    }
  };

  return (
    <div className="model-import">
      <p className="muted">{t("models.importHint")}</p>
      <label>
        <span className="field-label">{t("models.importJsonLabel")}</span>
        <textarea className="input" rows={8} value={importText} onChange={(event) => setImportText(event.target.value)} />
      </label>
      {error && <div className="error">{error}</div>}
      <div className="card-actions"><button type="button" className="btn sm" onClick={() => void runImport(true)}>{t("models.importPreview")}</button><button type="button" className="btn sm primary" onClick={() => void runImport(false)}>{t("models.importApply")}</button></div>
      {importResult && <div className="muted">{t("models.importSummary", { applied: importResult.applied.length, unchanged: importResult.unchanged.length, skipped: importResult.skipped.length, keys: importResult.affected_keys.length })}</div>}
    </div>
  );
}
