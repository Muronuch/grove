INSERT INTO items (id, title) VALUES (1, 'from the golden snapshot'), (2, 'seeded once')
ON DUPLICATE KEY UPDATE title = VALUES(title);
