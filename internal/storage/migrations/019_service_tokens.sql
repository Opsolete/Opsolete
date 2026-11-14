-- Service tokens: automation (the Terraform provider, scripts) acts with admin rights as the token itself, not as a
-- person. No user; an expiry only if one was given; names unique, so an audit entry token:<name> names one token.
ALTER TABLE api_tokens DROP CONSTRAINT api_tokens_scope_check;
ALTER TABLE api_tokens ADD CONSTRAINT api_tokens_scope_check CHECK (scope IN ('registry', 'user', 'service'));
CREATE UNIQUE INDEX api_tokens_service_name_idx ON api_tokens (name) WHERE scope = 'service';
