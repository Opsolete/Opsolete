-- Organization-wide settings: one row.
CREATE TABLE organization (
    id       boolean PRIMARY KEY DEFAULT true CHECK (id),
    currency text NOT NULL DEFAULT 'USD' CHECK (currency IN ('USD', 'EUR')) -- of Infracost's cost estimates
);
INSERT INTO organization DEFAULT VALUES;
