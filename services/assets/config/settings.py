"""Django settings, read from environment variables through typed settings.

Task 1.1 keeps this minimal: no apps, no admin and no migrations yet. The Drawing model,
the admin moderation queue and Celery arrive in task 3.1.
"""

import socket

from pydantic_settings import BaseSettings

from config.logs import logging_config

SERVICE_NAME = "assets"


class Env(BaseSettings):
    service_version: str = "dev"
    service_instance_id: str = socket.gethostname()
    log_level: str = "info"
    django_secret_key: str
    db_host: str = "postgres"
    db_port: int = 5432
    db_name: str = "assets"
    db_user: str = "assets"
    db_password: str = ""


env = Env()  # required fields (DJANGO_SECRET_KEY) come from the environment

SECRET_KEY = env.django_secret_key
DEBUG = False
# Requests only arrive through the gateway; AWS restricts this to the real host name (ADR 0003).
ALLOWED_HOSTS = ["*"]

INSTALLED_APPS: list[str] = []
MIDDLEWARE = [
    "django.middleware.security.SecurityMiddleware",
    "django.middleware.common.CommonMiddleware",
]
ROOT_URLCONF = "config.urls"
WSGI_APPLICATION = "config.wsgi.application"

# Django connects lazily, so the skeleton starts even before the database is used.
DATABASES = {
    "default": {
        "ENGINE": "django.db.backends.postgresql",
        "HOST": env.db_host,
        "PORT": env.db_port,
        "NAME": env.db_name,
        "USER": env.db_user,
        "PASSWORD": env.db_password,
        "CONN_MAX_AGE": 60,
    }
}

USE_TZ = True
TIME_ZONE = "UTC"

LOGGING = logging_config(SERVICE_NAME, env.service_version, env.log_level)
