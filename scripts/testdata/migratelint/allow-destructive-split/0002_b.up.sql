ALTER TABLE widgets
  ADD COLUMN label text,
-- allow-destructive: the column was never populated; see VZ-XXX
  DROP
  handle;
