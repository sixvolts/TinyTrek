FROM debian:bookworm

ARG DEBIAN_FRONTEND=noninteractive
# Parameterized to match the host user (§6). No hardcoded 'capt_redbeard'.
ARG USER=builder
ARG HOST_UID=1000
ARG HOST_GID=1000

# Yocto build host dependencies. Doc-toolchain packages (inkscape/texlive/sphinx)
# from the old Dockerfile are dropped -- not needed to build the image.
RUN apt-get update && apt-get install -yq \
        bash gawk wget git diffstat unzip texinfo gcc build-essential chrpath socat \
        cpio python3 python3-pip python3-pexpect xz-utils debianutils iputils-ping \
        python3-git python3-jinja2 python3-subunit zstd liblz4-tool file locales \
        libacl1 make sudo pipx tmux iproute2 iptables \
    && rm -rf /var/lib/apt/lists/*

# bitbake needs /bin/sh to be bash-compatible.
RUN ln -sf /bin/bash /bin/sh

RUN echo "en_US.UTF-8 UTF-8" > /etc/locale.gen && locale-gen en_US.UTF-8
ENV LANG=en_US.utf8

# Create a build user whose UID/GID match the host, so files written to the
# bind-mounted repo and /yocto volume are owned correctly.
RUN groupadd -g ${HOST_GID} ${USER} 2>/dev/null || true \
    && useradd -ms /bin/bash -u ${HOST_UID} -g ${HOST_GID} ${USER} \
    && usermod -aG sudo ${USER} \
    && echo "${USER} ALL=(ALL) NOPASSWD: ALL" >> /etc/sudoers

# NOTE (§4.3): the old container did `ssh-keyscan git.faro.re` and forwarded
# SSH_AUTH_SOCK to fetch a private repo. Both are removed -- the baseline has no
# private repo dependency and no committed credentials.

USER ${USER}
RUN pipx install kas
ENV PATH="/home/${USER}/.local/bin:${PATH}"

WORKDIR /work
