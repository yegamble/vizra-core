-- allow-destructive: r
ALTER TABLE sites
  DROP COLUMN handle,
  DROP COLUMN name;
