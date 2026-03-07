import { useState, useCallback } from "react";
import { useWs } from "@/hooks/use-ws";
import { useAuthStore } from "@/stores/use-auth-store";
import { Methods } from "@/api/protocol";
import type { AgentLinkData, AgentLinkSettings } from "@/types/agent";

export function useAgentLinks(agentId: string) {
  const ws = useWs();
  const connected = useAuthStore((s) => s.connected);
  const [links, setLinks] = useState<AgentLinkData[]>([]);
  const [loading, setLoading] = useState(true);

  const load = useCallback(async () => {
    if (!connected) return;
    setLoading(true);
    try {
      const res = await ws.call<{ links: AgentLinkData[]; count: number }>(
        Methods.AGENTS_LINKS_LIST,
        { agentId, direction: "all" },
      );
      setLinks(res.links ?? []);
    } catch {
      // ignore
    } finally {
      setLoading(false);
    }
  }, [ws, agentId, connected]);

  const createLink = useCallback(
    async (params: {
      targetAgent: string;
      direction: string;
      description?: string;
      maxConcurrent?: number;
      settings?: AgentLinkSettings;
    }) => {
      await ws.call(Methods.AGENTS_LINKS_CREATE, {
        sourceAgent: agentId,
        targetAgent: params.targetAgent,
        direction: params.direction,
        description: params.description,
        maxConcurrent: params.maxConcurrent,
        settings: params.settings,
      });
      load();
    },
    [ws, agentId, load],
  );

  const updateLink = useCallback(
    async (linkId: string, updates: Record<string, unknown>) => {
      await ws.call(Methods.AGENTS_LINKS_UPDATE, { linkId, ...updates });
      load();
    },
    [ws, load],
  );

  const deleteLink = useCallback(
    async (linkId: string) => {
      await ws.call(Methods.AGENTS_LINKS_DELETE, { linkId });
      load();
    },
    [ws, load],
  );

  return { links, loading, load, createLink, updateLink, deleteLink };
}
