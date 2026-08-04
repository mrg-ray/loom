-- 000020_tool_exec_admission_decision.up.sql
-- Add the audit verdict to tool_executions (HLD §5.6). admission_decision holds
-- the admission decision ("allow"|"deny"|"ask") for a call matched by an audit
-- binding, distinct from the run outcome. NULL means the call was not audited;
-- only non-NULL rows count as audit records (SC-004). Nullable, no default, so
-- ungoverned/legacy rows carry no decision. user_id RLS is inherited unchanged.
ALTER TABLE tool_executions
    ADD COLUMN IF NOT EXISTS admission_decision TEXT;
