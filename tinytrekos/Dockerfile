FROM debian:bookworm

ARG USER=capt_redbeard
ARG DEBIAN_FRONTEND=noninteractive

# Install the required packages
RUN apt-get update && \
    apt-get install -yq bash gawk wget git diffstat unzip texinfo gcc build-essential chrpath socat \
        cpio python3 python3-pip python3-pexpect xz-utils debianutils iputils-ping python3-git \
        python3-jinja2 python3-subunit zstd liblz4-tool file locales libacl1 git make inkscape \
        texlive-latex-extra sphinx python3-saneyaml python3-sphinx-rtd-theme sudo pipx tmux iproute2 iptables

RUN ln -sf /bin/bash /bin/sh

# Set the locale
RUN echo "en_US.UTF-8 UTF-8" > /etc/locale.gen && \
    locale-gen en_US.UTF-8

# Clean up the image
RUN rm -rf /var/lib/apt/lists/*

ENV LANG=en_US.utf8

RUN groupadd ${USER} -g 1000

RUN useradd -ms /bin/bash -p ${USER} ${USER} -u 1000 -g 1000 && \
    usermod -aG sudo ${USER} && \
    echo "${USER}:${USER}" | chpasswd

RUN echo "${USER} ALL=(ALL) NOPASSWD: ALL" >> /etc/sudoers
    
USER ${USER}

RUN pipx install kas

RUN mkdir ~/.ssh && ssh-keyscan -t ed25519 git.faro.re >> ~/.ssh/known_hosts

ENV PATH="/home/${USER}/.local/bin:${PATH}"

