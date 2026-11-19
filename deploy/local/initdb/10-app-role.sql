-- Local development only: the application role traderd connects as.
-- The owner role is the image's POSTGRES_USER (a superuser in this container).
CREATE ROLE trader_app LOGIN PASSWORD 'local-dev-only';
