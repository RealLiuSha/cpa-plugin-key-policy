import { ALL } from "../channelModels";
import { useT } from "../i18n";

type Translate = ReturnType<typeof useT>;

// A channel type is "<source>:<provider>": auth_files for CPA's auth files,
// ai_providers for the API keys of its config.
export function typeLabel(t: Translate, type: string): string {
  const [source, provider] = type.split(/:(.*)/);
  return source === "auth_files" || source === "ai_providers" ? t(`channels.filter.${source}`, { provider }) : type;
}

interface Props {
  types: string[];
  type: string;
  // The models to choose from with how many channels serve each; null while
  // they are looked up.
  models: { model: string; count: number }[] | null;
  model: string;
  onType: (type: string) => void;
  onModel: (model: string) => void;
}

// Narrows a channel list by type, then by a model the channels serve. A
// choice the list no longer has stays selectable, so the URL keeps it.
export default function ChannelFilter({ types, type, models, model, onType, onModel }: Props) {
  const t = useT();
  const typeOptions = type === ALL || types.includes(type) ? types : [type, ...types];
  const known = models ?? [];
  const modelOptions = model === ALL || known.some((item) => item.model === model) ? known : [{ model, count: 0 }, ...known];
  return (
    <div className="channel-filter">
      <select className="channel-filter-type" aria-label={t("channels.filter.type")} value={type} onChange={(event) => onType(event.target.value)}>
        <option value={ALL}>{t("channels.filter.allTypes")}</option>
        {typeOptions.map((value) => <option key={value} value={value}>{typeLabel(t, value)}</option>)}
      </select>
      <select className="channel-filter-model" aria-label={t("channels.filter.model")} value={model} disabled={models === null && model === ALL} onChange={(event) => onModel(event.target.value)}>
        <option value={ALL}>{t(models === null ? "channels.filter.loadingModels" : "channels.filter.allModels")}</option>
        {modelOptions.map((item) => (
          <option key={item.model} value={item.model}>{item.count > 0 ? t("channels.filter.modelOption", { model: item.model, count: item.count }) : item.model}</option>
        ))}
      </select>
    </div>
  );
}
