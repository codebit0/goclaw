import { useEffect } from "react";
import { useForm, Controller } from "react-hook-form";
import { zodResolver } from "@hookform/resolvers/zod";
import { useTranslation } from "react-i18next";
import {
  Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { hookFormSchema, type HookFormData } from "@/schemas/hooks.schema";
import type { HookConfig } from "@/hooks/use-hooks";

// TODO: replace with real edition context once EditionContext is available
const IS_LITE_EDITION = false;

const HOOK_EVENTS = [
  "session_start", "user_prompt_submit", "pre_tool_use",
  "post_tool_use", "stop", "subagent_start", "subagent_stop",
] as const;

interface HookFormDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSubmit: (data: HookFormData) => Promise<void>;
  initial?: HookConfig | null;
}

export function HookFormDialog({ open, onOpenChange, onSubmit, initial }: HookFormDialogProps) {
  const { t } = useTranslation("hooks");

  const {
    register, control, handleSubmit, watch, reset,
    formState: { errors, isSubmitting },
  } = useForm<HookFormData>({
    resolver: zodResolver(hookFormSchema),
    defaultValues: {
      event: "pre_tool_use",
      handler_type: "http",
      scope: "tenant",
      timeout_ms: 5000,
      on_timeout: "block",
      priority: 100,
      enabled: true,
      method: "POST",
      max_invocations_per_turn: 5,
    },
  });

  const handlerType = watch("handler_type");

  useEffect(() => {
    if (open) {
      if (initial) {
        const cfg = initial.config as Record<string, unknown>;
        reset({
          event: initial.event as HookFormData["event"],
          handler_type: initial.handler_type,
          scope: initial.scope,
          matcher: initial.matcher ?? "",
          if_expr: initial.if_expr ?? "",
          timeout_ms: initial.timeout_ms,
          on_timeout: initial.on_timeout,
          priority: initial.priority,
          enabled: initial.enabled,
          command: (cfg.command as string) ?? "",
          allowed_env_vars: ((cfg.allowed_env_vars as string[]) ?? []).join(","),
          cwd: (cfg.cwd as string) ?? "",
          url: (cfg.url as string) ?? "",
          method: (cfg.method as HookFormData["method"]) ?? "POST",
          headers: cfg.headers ? JSON.stringify(cfg.headers) : "",
          body_template: (cfg.body_template as string) ?? "",
          prompt_template: (cfg.prompt_template as string) ?? "",
          model: (cfg.model as string) ?? "",
          max_invocations_per_turn: (cfg.max_invocations_per_turn as number) ?? 5,
        });
      } else {
        reset();
      }
    }
  }, [open, initial, reset]);

  const onFormSubmit = async (data: HookFormData) => {
    await onSubmit(data);
    onOpenChange(false);
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[90vh] flex flex-col max-sm:inset-0 max-sm:rounded-none sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{initial ? t("form.title_edit") : t("form.title_create")}</DialogTitle>
        </DialogHeader>

        <div className="flex-1 space-y-4 overflow-y-auto -mx-4 px-4 sm:-mx-6 sm:px-6">
          {/* Event */}
          <div className="space-y-1.5">
            <Label>{t("form.event")}</Label>
            <Controller control={control} name="event" render={({ field }) => (
              <Select value={field.value} onValueChange={field.onChange}>
                <SelectTrigger className="text-base md:text-sm">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {HOOK_EVENTS.map((e) => (
                    <SelectItem key={e} value={e}>{e}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )} />
          </div>

          {/* Handler type */}
          <div className="space-y-1.5">
            <Label>{t("form.handlerType")}</Label>
            <Controller control={control} name="handler_type" render={({ field }) => (
              <RadioGroup value={field.value} onValueChange={field.onChange} className="flex gap-4">
                {(["command", "http", "prompt"] as const).map((ht) => {
                  const disabled = ht === "command" && !IS_LITE_EDITION;
                  const radio = (
                    <div key={ht} className={`flex items-center gap-1.5 ${disabled ? "opacity-50" : ""}`}>
                      <RadioGroupItem value={ht} id={`ht-${ht}`} disabled={disabled} />
                      <Label htmlFor={`ht-${ht}`} className={disabled ? "cursor-not-allowed" : "cursor-pointer"}>
                        {ht}
                      </Label>
                    </div>
                  );
                  if (disabled) {
                    return (
                      <TooltipProvider key={ht} delayDuration={200}>
                        <Tooltip>
                          <TooltipTrigger asChild><span>{radio}</span></TooltipTrigger>
                          <TooltipContent>{t("form.commandDisabledTooltip")}</TooltipContent>
                        </Tooltip>
                      </TooltipProvider>
                    );
                  }
                  return radio;
                })}
              </RadioGroup>
            )} />
          </div>

          {/* Scope */}
          <div className="space-y-1.5">
            <Label>{t("form.scope")}</Label>
            <Controller control={control} name="scope" render={({ field }) => (
              <Select value={field.value} onValueChange={field.onChange}>
                <SelectTrigger className="text-base md:text-sm">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {(["global", "tenant", "agent"] as const).map((s) => (
                    <SelectItem key={s} value={s}>{s}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            )} />
          </div>

          {/* Matcher */}
          <div className="space-y-1.5">
            <Label>{t("form.matcher")}</Label>
            <Input {...register("matcher")} placeholder="^bash$" className="text-base md:text-sm font-mono" />
            {errors.matcher
              ? <p className="text-xs text-destructive">{errors.matcher.message}</p>
              : <p className="text-xs text-muted-foreground">{t("form.matcherHint")}</p>
            }
          </div>

          {/* if_expr */}
          <div className="space-y-1.5">
            <Label>{t("form.ifExpr")}</Label>
            <Input {...register("if_expr")} placeholder='tool_input.path.startsWith("/etc")' className="text-base md:text-sm font-mono" />
            <p className="text-xs text-muted-foreground">{t("form.ifExprHint")}</p>
          </div>

          {/* Handler-specific sub-forms */}
          {handlerType === "command" && (
            <div className="space-y-3 rounded-lg border p-3">
              <div className="space-y-1.5">
                <Label>{t("form.command")}</Label>
                <Input {...register("command")} placeholder="/usr/local/bin/hook.sh" className="text-base md:text-sm font-mono" />
              </div>
              <div className="space-y-1.5">
                <Label>{t("form.allowedEnvVars")}</Label>
                <Input {...register("allowed_env_vars")} placeholder="PATH,HOME" className="text-base md:text-sm" />
              </div>
              <div className="space-y-1.5">
                <Label>{t("form.cwd")}</Label>
                <Input {...register("cwd")} placeholder="/tmp" className="text-base md:text-sm font-mono" />
              </div>
            </div>
          )}

          {handlerType === "http" && (
            <div className="space-y-3 rounded-lg border p-3">
              <div className="space-y-1.5">
                <Label>{t("form.url")}</Label>
                <Input {...register("url")} placeholder="https://hooks.example.com/agent" className="text-base md:text-sm" />
                {errors.url && <p className="text-xs text-destructive">{errors.url.message}</p>}
              </div>
              <div className="space-y-1.5">
                <Label>{t("form.method")}</Label>
                <Controller control={control} name="method" render={({ field }) => (
                  <Select value={field.value ?? "POST"} onValueChange={field.onChange}>
                    <SelectTrigger className="text-base md:text-sm">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {["GET", "POST", "PUT", "PATCH", "DELETE"].map((m) => (
                        <SelectItem key={m} value={m}>{m}</SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                )} />
              </div>
              <div className="space-y-1.5">
                <Label>{t("form.bodyTemplate")}</Label>
                <Textarea {...register("body_template")} rows={3} placeholder='{"event": "{{.Event}}"}' className="text-base md:text-sm font-mono" />
              </div>
            </div>
          )}

          {handlerType === "prompt" && (
            <div className="space-y-3 rounded-lg border p-3">
              <div className="space-y-1.5">
                <Label>{t("form.promptTemplate")}</Label>
                <Textarea
                  {...register("prompt_template")}
                  rows={4}
                  placeholder="Evaluate the tool call and decide whether to allow or block it."
                  className="text-base md:text-sm"
                />
                {errors.prompt_template && (
                  <p className="text-xs text-destructive">{errors.prompt_template.message}</p>
                )}
              </div>
              <div className="space-y-1.5">
                <Label>{t("form.model")}</Label>
                <Controller control={control} name="model" render={({ field }) => (
                  <Select value={field.value ?? "haiku"} onValueChange={field.onChange}>
                    <SelectTrigger className="text-base md:text-sm">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="haiku">haiku</SelectItem>
                      <SelectItem value="sonnet">sonnet</SelectItem>
                      <SelectItem value="opus">opus</SelectItem>
                    </SelectContent>
                  </Select>
                )} />
              </div>
              <div className="space-y-1.5">
                <Label>{t("form.maxInvocationsPerTurn")}</Label>
                <Input
                  type="number"
                  min={1}
                  max={20}
                  {...register("max_invocations_per_turn", { valueAsNumber: true })}
                  className="text-base md:text-sm w-24"
                />
              </div>
            </div>
          )}

          {/* Timeout + on_timeout */}
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-1.5">
              <Label>{t("form.timeout")}</Label>
              <Input
                type="number"
                min={100}
                {...register("timeout_ms", { valueAsNumber: true })}
                className="text-base md:text-sm"
              />
            </div>
            <div className="space-y-1.5">
              <Label>{t("form.onTimeout")}</Label>
              <Controller control={control} name="on_timeout" render={({ field }) => (
                <Select value={field.value} onValueChange={field.onChange}>
                  <SelectTrigger className="text-base md:text-sm">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="block">block</SelectItem>
                    <SelectItem value="allow">allow</SelectItem>
                  </SelectContent>
                </Select>
              )} />
            </div>
          </div>

          {/* Priority + enabled */}
          <div className="flex items-center gap-4">
            <div className="flex-1 space-y-1.5">
              <Label>{t("form.priority")}</Label>
              <Input
                type="number"
                min={0}
                max={1000}
                {...register("priority", { valueAsNumber: true })}
                className="text-base md:text-sm"
              />
            </div>
            <div className="space-y-1.5">
              <Label>{t("form.enabled")}</Label>
              <div className="flex h-9 items-center">
                <Controller control={control} name="enabled" render={({ field }) => (
                  <Switch checked={field.value} onCheckedChange={field.onChange} />
                )} />
              </div>
            </div>
          </div>
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)} disabled={isSubmitting}>
            {t("form.cancel")}
          </Button>
          <Button onClick={handleSubmit(onFormSubmit)} disabled={isSubmitting}>
            {t("form.save")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
