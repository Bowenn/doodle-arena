DELETE FROM delivery_attempts WHERE delivery_id IN (SELECT d.id FROM deliveries d JOIN endpoints e ON e.id = d.endpoint_id WHERE e.api_key_id IS NULL);
DELETE FROM deliveries WHERE endpoint_id IN (SELECT id FROM endpoints WHERE api_key_id IS NULL);
DELETE FROM endpoints WHERE api_key_id IS NULL;
ALTER TABLE endpoints ALTER COLUMN api_key_id SET NOT NULL;
