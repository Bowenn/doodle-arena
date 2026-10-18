-- Endpoints registered from the web app's settings page use the session's access token, not an
-- API key (task 4.8), so they have no api_key_id.
ALTER TABLE endpoints ALTER COLUMN api_key_id DROP NOT NULL;
