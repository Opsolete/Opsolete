-- A Terraform variable whose value is an HCL expression (a list, map, object, number or bool), as HCP's
-- "HCL" checkbox; otherwise the value is a string.
ALTER TABLE variables ADD COLUMN hcl boolean NOT NULL DEFAULT false;
ALTER TABLE variables ADD CONSTRAINT variables_hcl_terraform CHECK (NOT hcl OR category = 'terraform');
