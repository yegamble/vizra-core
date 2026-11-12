-- allow-destructive: r
ALTER TABLE sites
  DROP handle; ALTER TABLE users DROP COLUMN email;
