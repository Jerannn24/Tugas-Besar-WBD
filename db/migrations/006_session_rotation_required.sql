ALTER TABLE app.session_families
    ADD COLUMN rotation_required boolean NOT NULL DEFAULT false;
