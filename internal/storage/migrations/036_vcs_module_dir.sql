-- Folder of a linked module in its repository ('' = the root), so one repository can hold several modules.
ALTER TABLE vcs_modules ADD COLUMN dir text NOT NULL DEFAULT '';
