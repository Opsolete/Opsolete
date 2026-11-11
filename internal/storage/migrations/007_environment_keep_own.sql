-- keep_own: a workspace with its own value for the input (saved before the project set one, or an imported
-- resource's) keeps it; the others take the project's. false: the project's value applies to all (002).
ALTER TABLE environment_values ADD COLUMN keep_own boolean NOT NULL DEFAULT false;
