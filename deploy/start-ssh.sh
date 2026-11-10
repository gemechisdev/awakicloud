#!/bin/bash
set -e

# Start SSH server on port 22 (for TCP Proxy + SSH access)
mkdir -p /run/sshd

# Set root password from env var (SSH_ROOT_PASSWORD) if provided
if [ -n "$SSH_ROOT_PASSWORD" ]; then
    echo "root:${SSH_ROOT_PASSWORD}" | chpasswd
    echo "Root password set from SSH_ROOT_PASSWORD"
fi

/usr/sbin/sshd -e -p 22 &
echo "SSH started on port 22"

# Start the Awaki agent (the main service)
exec /usr/local/bin/awaki
