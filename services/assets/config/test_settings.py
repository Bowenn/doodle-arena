"""Settings for pytest: supplies the required secret key, then loads the real settings."""

import os

os.environ.setdefault("DJANGO_SECRET_KEY", "test-only-not-a-secret")

from config.settings import *  # noqa: F403

# No collectstatic in tests: WhiteNoise serves the admin's files from the app directories.
STATIC_ROOT = None
WHITENOISE_USE_FINDERS = True

# Presigning needs a key even though it never calls the store; tests that do call it use the
# test container's (tests/conftest.py, stores).
S3_ACCESS_KEY = "test-only"
S3_SECRET_KEY = "test-only-not-a-secret"

# The gallery cache needs Redis; the tests that use it turn it on with their test container's.
ASSETS_CACHE = False
