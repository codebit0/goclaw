package i18n

// Korean catalog. Only entries that need a Korean override are listed;
// everything else falls back to English via lookup() in i18n.go.
// Add more translations here as needed — keys not present here resolve
// to the English template automatically.
func init() {
	register(LocaleKO, map[string]string{
		// System prompt meta — drives the leading line and the cron meta block.
		// Locale-correct framing here is what nudges the model toward Korean
		// responses for ACP/MCP-bridged agents in cron/heartbeat contexts.
		MsgSysChannelGreetingDirect:      "당신은 %s에서 동작하는 개인 비서입니다 (1:1 채팅).",
		MsgSysChannelGreetingGroup:       "당신은 %s에서 동작하는 개인 비서입니다 (그룹 채팅).",
		MsgSysChannelGreetingGroupTitled: "당신은 %s에서 동작하는 개인 비서입니다 (그룹 채팅 \"%s\").",
		MsgSysCronJobMetaDeliver:         "[크론 작업]\n예약된 작업 \"%s\" (ID: %s)을 수행합니다.\n요청자: 사용자 %s, 채널 \"%s\" (채팅 %s).\n응답은 해당 채팅으로 자동 전달됩니다 — 내용만 작성하면 됩니다.",
		MsgSysCronJobMetaNoDeliver:       "[크론 작업]\n예약된 작업 \"%s\" (ID: %s), 사용자 %s가 등록함.\n전달 설정이 없으므로 평소대로 응답하세요.",
	})
}
