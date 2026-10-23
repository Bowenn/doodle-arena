#!/usr/bin/env bash
# Installs what `make up`, `make test` and the checks need on the remote dev box: a Debian 13
# (trixie) x86 VM on Google Compute Engine (docs/setup.md, "Remote dev box (GCP)"). Run it on the
# VM as your own user, which needs sudo. It is safe to run again, and bumps a tool whose pinned
# version below changed. Versions follow docs/setup.md section 2 and CI.
set -euo pipefail

GO_VERSION=1.27.2
NODE_VERSION=24.22.0
K6_VERSION=2.3.0
BUF_VERSION=1.74.0
UV_VERSION=0.13.1

if [[ "$(. /etc/os-release && echo "$VERSION_CODENAME")" != trixie || "$(uname -m)" != x86_64 ]]; then
  echo "This script is for Debian 13 (trixie) on x86_64." >&2
  exit 1
fi

echo "==> Debian packages"
sudo apt-get update -qq
# gcc: Go's -race detector needs cgo.
sudo apt-get install -y -qq ca-certificates curl gnupg git make gcc jq openssl python3 xz-utils unzip

# Docker Engine and the compose plugin, from Docker's own apt repository (Debian's docker.io
# has no compose v2 plugin).
if [[ ! -f /etc/apt/sources.list.d/docker.list ]]; then
  sudo install -m 0755 -d /etc/apt/keyrings
  sudo curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc
  echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian trixie stable" |
    sudo tee /etc/apt/sources.list.d/docker.list > /dev/null
fi
# Temurin 25 (the services) and 21 (analytics' Gradle toolchain, Flink's Java), from Adoptium.
if [[ ! -f /etc/apt/sources.list.d/adoptium.list ]]; then
  curl -fsSL https://packages.adoptium.net/artifactory/api/gpg/key/public |
    sudo gpg --dearmor --yes -o /etc/apt/keyrings/adoptium.gpg
  echo "deb [signed-by=/etc/apt/keyrings/adoptium.gpg] https://packages.adoptium.net/artifactory/deb trixie main" |
    sudo tee /etc/apt/sources.list.d/adoptium.list > /dev/null
fi
# The Stripe CLI forwards test-mode webhooks to the gateway (docs/setup.md, "Stripe").
if [[ ! -f /etc/apt/sources.list.d/stripe.list ]]; then
  curl -fsSL https://packages.stripe.dev/api/security/keypair/stripe-cli-gpg/public |
    sudo gpg --dearmor --yes -o /etc/apt/keyrings/stripe.gpg
  echo "deb [signed-by=/etc/apt/keyrings/stripe.gpg] https://packages.stripe.dev/stripe-cli-debian-local stable main" |
    sudo tee /etc/apt/sources.list.d/stripe.list > /dev/null
fi
echo "==> Docker Engine, compose plugin, Temurin 25 and 21, Stripe CLI"
sudo apt-get update -qq
sudo apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin \
  temurin-25-jdk temurin-21-jdk stripe
sudo update-java-alternatives --set temurin-25-jdk-amd64 > /dev/null 2>&1 || true
if ! id -nG | grep -qw docker; then
  sudo usermod -aG docker "$USER"
  echo "Added $USER to the docker group: log out and back in before using docker."
fi

# Tarball installs into /usr/local, replaced when the pinned version changes.
install_go() {
  if [[ "$(/usr/local/go/bin/go version 2>/dev/null)" == *"go$GO_VERSION "* ]]; then return; fi
  echo "==> Go $GO_VERSION"
  sudo rm -rf /usr/local/go
  curl -fsSL "https://go.dev/dl/go$GO_VERSION.linux-amd64.tar.gz" | sudo tar -C /usr/local -xz
}
install_node() {
  if [[ "$(/usr/local/node/bin/node --version 2>/dev/null)" == "v$NODE_VERSION" ]]; then return; fi
  echo "==> Node.js $NODE_VERSION"
  sudo rm -rf /usr/local/node && sudo mkdir -p /usr/local/node
  curl -fsSL "https://nodejs.org/dist/v$NODE_VERSION/node-v$NODE_VERSION-linux-x64.tar.xz" |
    sudo tar -C /usr/local/node --strip-components=1 -xJ
}
install_k6() {
  if [[ "$(k6 version 2>/dev/null)" == *"v$K6_VERSION "* ]]; then return; fi
  echo "==> k6 $K6_VERSION"
  curl -fsSL "https://github.com/grafana/k6/releases/download/v$K6_VERSION/k6-v$K6_VERSION-linux-amd64.tar.gz" |
    sudo tar -C /usr/local/bin --strip-components=1 -xz "k6-v$K6_VERSION-linux-amd64/k6"
}
install_buf() {
  if [[ "$(buf --version 2>/dev/null)" == "$BUF_VERSION" ]]; then return; fi
  echo "==> buf $BUF_VERSION"
  sudo curl -fsSL -o /usr/local/bin/buf "https://github.com/bufbuild/buf/releases/download/v$BUF_VERSION/buf-Linux-x86_64"
  sudo chmod 755 /usr/local/bin/buf
}
install_uv() {
  if [[ "$("$HOME/.local/bin/uv" --version 2>/dev/null)" == "uv $UV_VERSION"* ]]; then return; fi
  echo "==> uv $UV_VERSION"
  curl -fsSL "https://astral.sh/uv/$UV_VERSION/install.sh" | env UV_NO_MODIFY_PATH=1 sh -s -- --quiet
}
install_go
install_node
install_k6
install_buf
install_uv

# Links in /usr/local/bin rather than PATH in ~/.profile: `ssh devbox <command>` and Claude Code's
# SSH sessions run non-login shells, which never read ~/.profile.
for bin in /usr/local/go/bin/go /usr/local/go/bin/gofmt /usr/local/node/bin/node /usr/local/node/bin/npm \
  /usr/local/node/bin/npx "$HOME/.local/bin/uv" "$HOME/.local/bin/uvx"; do
  sudo ln -sf "$bin" /usr/local/bin/
done

cat <<'EOF'

Toolchains installed. Log out and back in (for the docker group), then:
  git clone git@github.com:Bowenn/gap-project_private.git doodle-arena && cd doodle-arena && make up
EOF
