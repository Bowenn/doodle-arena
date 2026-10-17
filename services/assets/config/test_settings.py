"""Settings for pytest: supplies the required secret key, then loads the real settings."""

import os

os.environ.setdefault("DJANGO_SECRET_KEY", "test-only-not-a-secret")

from config.settings import *  # noqa: F403

# No collectstatic in tests: WhiteNoise serves the admin's files from the app directories.
STATIC_ROOT = None
WHITENOISE_USE_FINDERS = True
