-- User tokens from `terraform login`: they act as that user (workspace access, the cloud block)
-- and also work for the registry. Registry and runner tokens have no user and don't expire.
ALTER TABLE api_tokens
    ADD COLUMN user_id    uuid REFERENCES users ON DELETE CASCADE,
    ADD COLUMN expires_at timestamptz,
    DROP CONSTRAINT api_tokens_scope_check,
    ADD CONSTRAINT api_tokens_scope_check CHECK (scope IN ('registry', 'runner', 'user')),
    ADD CONSTRAINT api_tokens_user_check CHECK ((scope = 'user') = (user_id IS NOT NULL AND expires_at IS NOT NULL));
