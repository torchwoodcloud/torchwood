DROP TABLE IF EXISTS invite_codes;

ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_registration_policy_check;
ALTER TABLE projects DROP COLUMN IF EXISTS registration_policy;
