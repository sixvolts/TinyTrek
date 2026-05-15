#!/bin/sh

# Create necessary directories on /data partition
mkdir -p /data/overlay/ssh-upper
mkdir -p /data/overlay/ssh-work

# Copy existing SSH keys if not already done
if [ ! "$(ls -A /data/overlay/ssh-upper)" ]; then
    cp -r /etc/ssh/* /data/overlay/ssh-upper/
fi

# Mount OverlayFS
mount -t overlay overlay -o lowerdir=/etc/ssh,upperdir=/data/overlay/ssh-upper,workdir=/data/overlay/ssh-work /etc/ssh
