import { useState } from "react";
import { useTranslation } from "react-i18next";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Badge } from "@/components/ui/badge";
import { useTestHook, type HookConfig, type HookTestResult } from "@/hooks/use-hooks";
import { HookDiffViewer } from "./hook-diff-viewer";

interface HookTestPanelProps {
  hook: Partial<HookConfig>;
}

const DECISION_STYLES: Record<string, string> = {
  allow: "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300",
  block: "bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300",
  error: "bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300",
  timeout: "bg-slate-100 text-slate-700 dark:bg-slate-800 dark:text-slate-300",
};

const LS_KEY = (id: string) => `goclaw:hook-test:${id}`;

function loadSavedSample(id: string | undefined) {
  if (!id) return null;
  try {
    const raw = localStorage.getItem(LS_KEY(id));
    return raw ? JSON.parse(raw) : null;
  } catch {
    return null;
  }
}

function saveSample(id: string | undefined, data: unknown) {
  if (!id) return;
  try {
    localStorage.setItem(LS_KEY(id), JSON.stringify(data));
  } catch {
    // ignore quota errors
  }
}

export function HookTestPanel({ hook }: HookTestPanelProps) {
  const { t } = useTranslation("hooks");
  const testMutation = useTestHook();

  const saved = loadSavedSample(hook.id);
  const [toolName, setToolName] = useState<string>(saved?.toolName ?? "bash");
  const [toolInputRaw, setToolInputRaw] = useState<string>(
    saved?.toolInputRaw ?? JSON.stringify({ command: "ls -la" }, null, 2),
  );
  const [rawInput, setRawInput] = useState<string>(saved?.rawInput ?? "");
  const [result, setResult] = useState<HookTestResult | null>(null);
  const [parseError, setParseError] = useState<string | null>(null);

  const handleFire = async () => {
    setParseError(null);
    let toolInput: Record<string, unknown>;
    try {
      toolInput = JSON.parse(toolInputRaw);
    } catch {
      setParseError("Tool input must be valid JSON");
      return;
    }

    saveSample(hook.id, { toolName, toolInputRaw, rawInput });

    const res = await testMutation.mutateAsync({
      config: hook,
      sampleEvent: { toolName, toolInput, rawInput: rawInput || undefined },
    });
    setResult(res.result);
  };

  return (
    <div className="space-y-4">
      <p className="text-sm font-medium">{t("test.sampleEvent")}</p>

      {/* Tool name */}
      <div className="space-y-1.5">
        <Label className="text-xs">{t("test.toolName")}</Label>
        <Input
          value={toolName}
          onChange={(e) => setToolName(e.target.value)}
          placeholder="bash"
          className="text-base md:text-sm font-mono"
        />
      </div>

      {/* Tool input */}
      <div className="space-y-1.5">
        <Label className="text-xs">{t("test.toolInput")}</Label>
        <Textarea
          value={toolInputRaw}
          onChange={(e) => setToolInputRaw(e.target.value)}
          rows={5}
          placeholder='{"command": "ls -la"}'
          className="text-base md:text-sm font-mono"
        />
        {parseError && <p className="text-xs text-destructive">{parseError}</p>}
      </div>

      {/* Raw input (optional) */}
      <div className="space-y-1.5">
        <Label className="text-xs">{t("test.rawInput")}</Label>
        <Input
          value={rawInput}
          onChange={(e) => setRawInput(e.target.value)}
          placeholder="Optional raw user message"
          className="text-base md:text-sm"
        />
      </div>

      <Button
        onClick={handleFire}
        disabled={testMutation.isPending}
        size="sm"
        className="gap-1.5"
      >
        {testMutation.isPending ? t("test.firing") : t("test.fire")}
      </Button>

      {/* Result */}
      {result && (
        <div className="space-y-3 rounded-lg border p-4">
          <div className="flex flex-wrap items-center gap-3">
            <div>
              <span className="text-xs text-muted-foreground">{t("test.decision")}: </span>
              <span
                className={`inline-flex items-center rounded px-2 py-0.5 text-xs font-semibold ${DECISION_STYLES[result.decision] ?? "bg-muted text-muted-foreground"}`}
              >
                {t(`decision.${result.decision}`)}
              </span>
            </div>
            <div className="text-xs text-muted-foreground">
              {t("test.duration")}: <span className="font-mono">{result.durationMs}ms</span>
            </div>
            {result.statusCode != null && (
              <div className="text-xs text-muted-foreground">
                {t("test.statusCode")}: <Badge variant="outline">{result.statusCode}</Badge>
              </div>
            )}
          </div>

          {result.reason && (
            <p className="text-sm text-muted-foreground">{result.reason}</p>
          )}

          {result.stdout && (
            <div className="space-y-1">
              <p className="text-xs font-medium">{t("test.stdout")}</p>
              <pre className="overflow-x-auto rounded bg-muted p-2 text-xs">{result.stdout}</pre>
            </div>
          )}

          {result.stderr && (
            <div className="space-y-1">
              <p className="text-xs font-medium text-destructive">{t("test.stderr")}</p>
              <pre className="overflow-x-auto rounded bg-destructive/10 p-2 text-xs text-destructive">{result.stderr}</pre>
            </div>
          )}

          {result.error && (
            <p className="text-xs text-destructive">{result.error}</p>
          )}

          {result.updatedInput && (
            <HookDiffViewer
              before={(() => {
                try { return JSON.parse(toolInputRaw); } catch { return {}; }
              })()}
              after={result.updatedInput}
            />
          )}
        </div>
      )}
    </div>
  );
}
