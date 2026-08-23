-- Revert 0005_maturity_numeric.
ALTER TABLE outputs ALTER COLUMN maturity TYPE BIGINT;
ALTER TABLE outputs ALTER COLUMN maturity SET DEFAULT 0;
