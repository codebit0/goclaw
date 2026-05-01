package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/nextlevelbuilder/goclaw/internal/agent"
	"github.com/nextlevelbuilder/goclaw/internal/bus"
	"github.com/nextlevelbuilder/goclaw/internal/channels"
	"github.com/nextlevelbuilder/goclaw/internal/config"
	"github.com/nextlevelbuilder/goclaw/internal/i18n"
	"github.com/nextlevelbuilder/goclaw/internal/scheduler"
	"github.com/nextlevelbuilder/goclaw/internal/sessions"
	"github.com/nextlevelbuilder/goclaw/internal/store"
)

// resolveCronLocale walks the user → tenant cascade defined for cron-triggered
// agent runs:
//
//	1) tenant_users.metadata->>'locale'  (job.UserID × job.TenantID)
//	2) tenants.settings->>'locale'       (tenant default)
//	3) "" (caller is responsible for falling back to i18n.DefaultLocale)
//
// The cascade reuses jsonb metadata fields that already exist on tenant_users
// and tenants — no schema changes, no env vars.
func resolveCronLocale(ctx context.Context, tenantStore store.TenantStore, tenantID uuid.UUID, userID string) string {
	if tenantStore == nil || tenantID == uuid.Nil {
		return ""
	}
	// 1) tenant_users.metadata
	if userID != "" {
		if tus, err := tenantStore.ListUserTenants(ctx, userID); err == nil {
			for _, tu := range tus {
				if tu.TenantID != tenantID {
					continue
				}
				if loc := jsonbLocale(tu.Metadata); loc != "" {
					return loc
				}
				break
			}
		}
	}
	// 2) tenants.settings
	if t, err := tenantStore.GetTenant(ctx, tenantID); err == nil && t != nil {
		if loc := jsonbLocale(t.Settings); loc != "" {
			return loc
		}
	}
	return ""
}

// jsonbLocale extracts a "locale" string from a jsonb blob. Returns "" on
// missing key, parse error, or non-string value.
func jsonbLocale(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var bag struct {
		Locale string `json:"locale"`
	}
	if json.Unmarshal(raw, &bag) != nil {
		return ""
	}
	return bag.Locale
}

// makeCronJobHandler creates a cron job handler that routes through the scheduler's cron lane.
// This ensures per-session concurrency control (same job can't run concurrently)
// and integration with /stop, /stopall commands.
// cronHeartbeatWakeFn holds the heartbeat wake function, set after ticker creation.
// Safe because cron jobs only fire after Start(), well after this is set.
var cronHeartbeatWakeFn func(agentID string)

func makeCronJobHandler(sched *scheduler.Scheduler, msgBus *bus.MessageBus, cfg *config.Config, channelMgr *channels.Manager, sessionMgr store.SessionStore, agentStore store.AgentStore, tenantStore store.TenantStore) func(job *store.CronJob) (*store.CronJobResult, error) {
	return func(job *store.CronJob) (*store.CronJobResult, error) {
		agentID := job.AgentID
		if agentID == "" && agentStore != nil {
			// Resolve real default agent from DB instead of using literal "default" string.
			tenantCtx := store.WithTenantID(context.Background(), job.TenantID)
			if defaultAgent, err := agentStore.GetDefault(tenantCtx); err == nil {
				agentID = defaultAgent.AgentKey
			} else {
				agentID = cfg.ResolveDefaultAgentID()
			}
		} else if agentID == "" {
			agentID = cfg.ResolveDefaultAgentID()
		} else if id, err := uuid.Parse(agentID); err == nil && agentStore != nil {
			// Resolve agentKey from UUID so session key uses agentKey
			// (consistent with chat/WS/team paths, fixes cache invalidation mismatch).
			cronCtx := store.WithTenantID(context.Background(), job.TenantID)
			if ag, err := agentStore.GetByID(cronCtx, id); err == nil {
				agentID = ag.AgentKey
			}
		} else {
			agentID = config.NormalizeAgentID(agentID)
		}

		sessionKey := sessions.BuildCronSessionKey(agentID, job.ID)
		channel := job.DeliverChannel
		if channel == "" {
			channel = "cron"
		}

		// Infer peer kind from the stored session metadata (group chats need it
		// so that tools like message can route correctly via group APIs).
		peerKind := resolveCronPeerKind(job)

		// Resolve channel type for system prompt context.
		channelType := resolveChannelType(channelMgr, channel)

		// Build context with tenant scope and timeout so agent loop events are
		// scoped correctly and a hung agent can't block the cron scheduler forever.
		jobTimeout := cfg.Cron.JobTimeoutDuration()
		cronCtx, cancelCron := context.WithTimeout(context.Background(), jobTimeout)
		defer cancelCron()
		cronCtx = store.WithTenantID(cronCtx, job.TenantID)

		// Cron has no inbound channel locale (unlike WS connect or HTTP
		// Accept-Language). Resolve via tenant_users → tenants cascade so the
		// system prompt and meta lines render in the right language. Empty
		// locale falls through to i18n.DefaultLocale (English) inside i18n.T.
		cronLocale := resolveCronLocale(cronCtx, tenantStore, job.TenantID, job.UserID)
		if cronLocale != "" {
			cronCtx = store.WithLocale(cronCtx, cronLocale)
		}

		// Build cron context so the agent knows delivery target and requester.
		var extraPrompt string
		if job.Deliver && job.DeliverChannel != "" && job.DeliverTo != "" {
			extraPrompt = i18n.T(cronLocale, i18n.MsgSysCronJobMetaDeliver,
				job.Name, job.ID, job.UserID, job.DeliverChannel, job.DeliverTo,
			)
		} else {
			extraPrompt = i18n.T(cronLocale, i18n.MsgSysCronJobMetaNoDeliver,
				job.Name, job.ID, job.UserID,
			)
		}

		// Reset session before each cron run to prevent tool errors from previous
		// runs from polluting the context and blocking future executions (#294).
		// Save() persists the empty session to DB so stale data won't reload after restart.
		// Stateless jobs skip this — they intentionally carry no session history.
		if !job.Stateless {
			sessionMgr.Reset(cronCtx, sessionKey)
			sessionMgr.Save(cronCtx, sessionKey)
		}

		// Schedule through cron lane — scheduler handles agent resolution and concurrency
		outCh := sched.Schedule(cronCtx, scheduler.LaneCron, agent.RunRequest{
			SessionKey:        sessionKey,
			Message:           job.Payload.Message,
			Channel:           channel,
			ChannelType:       channelType,
			ChatID:            job.DeliverTo,
			PeerKind:          peerKind,
			UserID:            job.UserID,
			RunID:             fmt.Sprintf("cron:%s", job.ID),
			Stream:            false,
			ExtraSystemPrompt: extraPrompt,
			TraceName:         fmt.Sprintf("Cron [%s] - %s", job.Name, agentID),
			TraceTags:         []string{"cron"},
		})

		// Block until the scheduled run completes or the timeout fires.
		var outcome scheduler.RunOutcome
		select {
		case outcome = <-outCh:
		case <-cronCtx.Done():
			return nil, fmt.Errorf("cron job %s timed out after %s", job.Name, jobTimeout)
		}
		if outcome.Err != nil {
			return nil, outcome.Err
		}

		result := outcome.Result

		// If job wants delivery to a channel, send the agent response to the target chat.
		if job.Deliver && job.DeliverChannel != "" && job.DeliverTo != "" {
			outMsg := bus.OutboundMessage{
				Channel: job.DeliverChannel,
				ChatID:  job.DeliverTo,
				Content: result.Content,
			}
			if peerKind == "group" {
				outMsg.Metadata = map[string]string{"group_id": job.DeliverTo}
			}
			appendMediaToOutbound(&outMsg, result.Media)
			msgBus.PublishOutbound(outMsg)
		} else if job.Deliver {
			slog.Warn("cron: delivery configured but channel/chatID missing — output discarded",
				"job_id", job.ID, "job_name", job.Name, "channel", job.DeliverChannel, "to", job.DeliverTo)
		}

		cronResult := &store.CronJobResult{
			Content: result.Content,
		}
		if result.Usage != nil {
			cronResult.InputTokens = result.Usage.PromptTokens
			cronResult.OutputTokens = result.Usage.CompletionTokens
		}

		// wakeMode: trigger heartbeat after cron job completes.
		// Use original job.AgentID (UUID) — cronHeartbeatWakeFn expects UUID for ticker.Wake().
		if job.WakeHeartbeat && cronHeartbeatWakeFn != nil {
			cronHeartbeatWakeFn(job.AgentID)
		}

		return cronResult, nil
	}
}

// resolveCronPeerKind infers peer kind from the cron job's user ID.
// Group cron jobs have userID prefixed with "group:" or "guild:" (set during job creation).
func resolveCronPeerKind(job *store.CronJob) string {
	if strings.HasPrefix(job.UserID, "group:") || strings.HasPrefix(job.UserID, "guild:") {
		return "group"
	}
	return ""
}
