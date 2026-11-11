-- allow-destructive: the column was never read; removed after two releases
ALTER TABLE sites
  DROP
  handle;
