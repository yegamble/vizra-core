-- allow-destructive: r
ALTER TABLE sites
  DROP handle, DROP CONSTRAINT sites_pkey;
