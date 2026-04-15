import { useTranslation } from "react-i18next";
import { Badge } from "@/components/ui/badge";
import type { HookConfig } from "@/hooks/use-hooks";

interface HookOverviewTabProps {
  hook: HookConfig;
}

function InfoRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-start gap-3 py-2 border-b last:border-b-0">
      <span className="w-36 shrink-0 text-xs text-muted-foreground">{label}</span>
      <span className="text-xs">{children}</span>
    </div>
  );
}

export function HookOverviewTab({ hook }: HookOverviewTabProps) {
  const { t } = useTranslation("hooks");

  return (
    <div className="space-y-4">
      <div className="rounded-lg border p-4">
        <InfoRow label={t("table.event")}>
          <span className="font-mono">{hook.event}</span>
        </InfoRow>
        <InfoRow label={t("table.type")}>
          <Badge variant="outline">{hook.handler_type}</Badge>
        </InfoRow>
        <InfoRow label={t("table.scope")}>
          <Badge variant="outline">{hook.scope}</Badge>
        </InfoRow>
        {hook.matcher && (
          <InfoRow label={t("table.matcher")}>
            <code className="font-mono text-xs bg-muted px-1.5 py-0.5 rounded">{hook.matcher}</code>
          </InfoRow>
        )}
        {hook.if_expr && (
          <InfoRow label={t("form.ifExpr")}>
            <code className="font-mono text-xs bg-muted px-1.5 py-0.5 rounded">{hook.if_expr}</code>
          </InfoRow>
        )}
        <InfoRow label={t("form.timeout")}>
          <span className="font-mono">{hook.timeout_ms}ms</span>
          {" · "}
          <span className="text-muted-foreground">{t("form.onTimeout")}: {hook.on_timeout}</span>
        </InfoRow>
        <InfoRow label={t("form.priority")}>
          <span className="font-mono">{hook.priority}</span>
        </InfoRow>
        <InfoRow label={t("table.enabled")}>
          <span className={hook.enabled ? "text-emerald-600" : "text-muted-foreground"}>
            {hook.enabled ? "Yes" : "No"}
          </span>
        </InfoRow>
        <InfoRow label="Source">
          <Badge variant="secondary">{hook.source}</Badge>
        </InfoRow>
        <InfoRow label="Version">
          <span className="font-mono">v{hook.version}</span>
        </InfoRow>
        <InfoRow label="Created">
          <span>{new Date(hook.created_at).toLocaleString()}</span>
        </InfoRow>
        <InfoRow label="Updated">
          <span>{new Date(hook.updated_at).toLocaleString()}</span>
        </InfoRow>
      </div>

      {/* Config preview */}
      {hook.config && Object.keys(hook.config).length > 0 && (
        <div className="space-y-1.5">
          <p className="text-xs font-medium text-muted-foreground">{t("tabs.config")}</p>
          <pre className="overflow-x-auto rounded border bg-muted/40 p-3 text-xs font-mono">
            {JSON.stringify(hook.config, null, 2)}
          </pre>
        </div>
      )}
    </div>
  );
}
