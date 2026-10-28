-- The OIDC identity (issuer + sub) a user is bound to: emails can be reassigned, subjects aren't.
-- Existing users are bound at their next login by email. NULL: the dev user, or not yet bound.
ALTER TABLE users ADD COLUMN subject text UNIQUE;
