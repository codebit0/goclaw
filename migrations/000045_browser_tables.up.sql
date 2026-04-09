-- browser_proxies: proxy pool per tenant
CREATE TABLE IF NOT EXISTS browser_proxies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    url TEXT NOT NULL,
    username TEXT,
    password TEXT,
    geo TEXT,
    tags TEXT[] DEFAULT '{}',
    is_enabled BOOLEAN NOT NULL DEFAULT true,
    is_healthy BOOLEAN DEFAULT true,
    last_health_check TIMESTAMPTZ,
    fail_count INT DEFAULT 0,
    created_at TIMESTAMPTZ DEFAULT now(),
    updated_at TIMESTAMPTZ DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_browser_proxies_tenant ON browser_proxies(tenant_id);
CREATE INDEX IF NOT EXISTS idx_browser_proxies_healthy ON browser_proxies(tenant_id, is_healthy, geo);

-- browser_extensions: CRX/unpacked extensions
CREATE TABLE IF NOT EXISTS browser_extensions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    name TEXT NOT NULL,
    path TEXT NOT NULL,
    enabled BOOLEAN DEFAULT true,
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_browser_extensions_tenant ON browser_extensions(tenant_id);

-- screencast_sessions: live view tokens
CREATE TABLE IF NOT EXISTS screencast_sessions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    token TEXT NOT NULL UNIQUE,
    target_id TEXT NOT NULL,
    mode TEXT NOT NULL DEFAULT 'view',
    created_by UUID,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_screencast_sessions_token ON screencast_sessions(token);
CREATE INDEX IF NOT EXISTS idx_screencast_sessions_tenant ON screencast_sessions(tenant_id);

-- browser_audit_log: all browser actions
CREATE TABLE IF NOT EXISTS browser_audit_log (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    user_id UUID,
    agent_id UUID,
    session_id UUID,
    action TEXT NOT NULL,
    target_id TEXT,
    args JSONB,
    result TEXT,
    error_text TEXT,
    duration_ms INT,
    created_at TIMESTAMPTZ DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_browser_audit_tenant ON browser_audit_log(tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_browser_audit_session ON browser_audit_log(session_id, created_at DESC);

-- proxy_profile_assignments: sticky proxy-to-profile mapping
CREATE TABLE IF NOT EXISTS proxy_profile_assignments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL,
    proxy_id UUID NOT NULL REFERENCES browser_proxies(id) ON DELETE CASCADE,
    profile_dir TEXT NOT NULL,
    assigned_at TIMESTAMPTZ DEFAULT now(),
    last_used_at TIMESTAMPTZ DEFAULT now(),
    UNIQUE(tenant_id, profile_dir)
);
CREATE INDEX IF NOT EXISTS idx_proxy_profile_tenant ON proxy_profile_assignments(tenant_id);
CREATE INDEX IF NOT EXISTS idx_proxy_profile_proxy ON proxy_profile_assignments(proxy_id);
