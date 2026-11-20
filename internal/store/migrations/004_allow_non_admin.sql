ALTER TABLE configuration ADD COLUMN allow_non_admin INTEGER NOT NULL DEFAULT 0 CHECK (allow_non_admin IN (0, 1));
