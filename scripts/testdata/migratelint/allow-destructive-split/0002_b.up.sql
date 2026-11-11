-- allow-destructive: the column was never populated; see VZ-XXX
ALTER TABLE widgets
  ADD COLUMN label text,
  DROP
  handle;
