INSERT INTO items (id, title)
VALUES (1, 'from the golden snapshot'), (2, 'seeded once')
ON CONFLICT (id) DO UPDATE SET title = EXCLUDED.title;

SELECT setval(pg_get_serial_sequence('items', 'id'), GREATEST((SELECT MAX(id) FROM items), 1));
