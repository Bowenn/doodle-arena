"""Alembic environment: runs migrations against the database from app.settings, or against
a connection passed in by tests (config.attributes["connection"])."""

import logging.config

from alembic import context
from sqlalchemy import Connection, create_engine

from app import SERVICE_NAME
from app.logs import logging_config
from app.settings import Settings

settings = Settings()
if context.config.attributes.get("connection") is None:
    # Running as the migrate job: log JSON like the service does.
    logging.config.dictConfig(
        logging_config(SERVICE_NAME, settings.service_version, settings.log_level)
    )


def run(connection: Connection) -> None:
    context.configure(connection=connection, transaction_per_migration=True)
    with context.begin_transaction():
        context.run_migrations()


connection = context.config.attributes.get("connection")
if connection is not None:
    run(connection)
else:
    engine = create_engine(settings.database_url())
    with engine.connect() as conn:
        run(conn)
    engine.dispose()
