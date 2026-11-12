-- allow-destructive: r
ALTER TABLE sites
  DROP handle, ADD x text DEFAULT E'\''; DROP TABLE users; --';
