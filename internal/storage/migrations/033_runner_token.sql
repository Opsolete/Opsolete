-- Runners now authenticate with the shared RUNNER_TOKEN from the server's config.
DELETE FROM api_tokens WHERE scope = 'runner';
ALTER TABLE api_tokens
    DROP CONSTRAINT api_tokens_scope_check,
    ADD CONSTRAINT api_tokens_scope_check CHECK (scope IN ('registry', 'user'));
