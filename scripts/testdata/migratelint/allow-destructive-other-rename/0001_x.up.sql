-- allow-destructive: r
ALTER TABLE sites
  DROP handle; ALTER TABLE users RENAME TO u2;
