COMMENT ON TABLE widgets IS 'alter table widgets drop handle; it''s prose';
/* ALTER TABLE widgets
   DROP handle; */
DO $body$ BEGIN PERFORM 1; END $body$;
ALTER TABLE widgets ADD COLUMN "drop" text;
