ALTER TABLE widgets
  ADD COLUMN label text,
  DROP
  COLUMN IF EXISTS handle;
