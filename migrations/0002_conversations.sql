-- D22: the shell reaches a run or a conversation through the invocation that started it; the result of a
-- host.run invocation names the run ("id") and the thread ("threadId").
CREATE INDEX action_invocations_host_run ON action_invocations ((result->>'id')) WHERE target->>'kind' = 'host.run';
CREATE INDEX action_invocations_host_thread ON action_invocations (workspace_id, subject, (result->>'threadId')) WHERE target->>'kind' = 'host.run';
