-- allow-destructive: r
ALTER TABLE sites
  DROP handle, ADD n text DEFAULT 'DROP TABLE users';
